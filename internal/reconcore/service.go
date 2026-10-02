package reconcore

import (
	"fmt"
	"sort"
	"time"
)

// Service implements the ReconCore contract on top of a Store. The functional
// currency is fixed for the lifetime of the process and is the unit every
// journal is balanced in.
type Service struct {
	store              *Store
	functionalCurrency string
	clock              func() time.Time
}

// NewService wires a store, a functional currency and an injectable clock.
func NewService(store *Store, functionalCurrency string, clock func() time.Time) (*Service, error) {
	if store == nil {
		return nil, InternalError("service requires a store")
	}
	code, err := validateCurrency(functionalCurrency, "functional currency", true)
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	return &Service{store: store, functionalCurrency: code, clock: clock}, nil
}

// FunctionalCurrency reports the reporting currency of this process.
func (s *Service) FunctionalCurrency() string { return s.functionalCurrency }

func (s *Service) now() string {
	return s.clock().UTC().Format("2006-01-02T15:04:05Z")
}

// runIdempotent executes action at most once per key. The first response is
// stored and replayed for every later request that carries the same key.
func (s *Service) runIdempotent(key, operation string, action func(state *State) (any, error)) (any, error) {
	if key == "" {
		return nil, ValidationError("Idempotency-Key header is required")
	}
	if len(key) > 200 {
		return nil, ValidationError("Idempotency-Key must be at most 200 characters")
	}
	var response any
	err := s.store.Update(func(state *State) error {
		if record, found := state.Idempotency[key]; found {
			if record.Operation != operation {
				return ConflictError("idempotency key was already used for another operation")
			}
			stored, err := decodeResponse(record.Response)
			if err != nil {
				return err
			}
			response = stored
			return nil
		}
		value, err := action(state)
		if err != nil {
			return err
		}
		normalized, err := normalizeResponse(value)
		if err != nil {
			return err
		}
		encoded, err := encodeResponse(normalized)
		if err != nil {
			return err
		}
		state.Idempotency[key] = &IdempotencyRecord{
			Key:       key,
			Operation: operation,
			Response:  encoded,
			CreatedAt: s.now(),
		}
		response = normalized
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

type createAccountRequest struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	ParentID *string `json:"parent_id"`
	Currency string  `json:"currency"`
}

// CreateAccount adds one node to the chart of accounts.
func (s *Service) CreateAccount(body []byte, key string) (any, error) {
	var request createAccountRequest
	if err := decodeObject(body, &request); err != nil {
		return nil, err
	}
	id, err := validateIdentifier(request.ID, "account id")
	if err != nil {
		return nil, err
	}
	name, err := validateText(request.Name, "name")
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ValidationError("name is required")
	}
	normalBalance, known := normalBalanceByType[request.Type]
	if !known {
		return nil, ValidationError("type must be one of asset, liability, equity, revenue or expense")
	}
	currency, err := validateCurrency(request.Currency, "currency", true)
	if err != nil {
		return nil, err
	}
	var parentID *string
	if request.ParentID != nil {
		value, err := validateIdentifier(*request.ParentID, "parent_id")
		if err != nil {
			return nil, err
		}
		parentID = &value
	}
	return s.runIdempotent(key, "create-account:"+id, func(state *State) (any, error) {
		if _, found := state.Accounts[id]; found {
			return nil, ConflictError("account %s already exists", id)
		}
		if parentID != nil {
			if _, found := state.Accounts[*parentID]; !found {
				return nil, ValidationError("parent_id %s does not exist", *parentID)
			}
		}
		account := &Account{
			ID:            id,
			Name:          name,
			Type:          request.Type,
			ParentID:      parentID,
			Currency:      currency,
			NormalBalance: normalBalance,
			CreatedAt:     s.now(),
		}
		state.Accounts[id] = account
		return account, nil
	})
}

// ListAccounts returns the whole chart of accounts as a forest.
func (s *Service) ListAccounts() (any, error) {
	return s.store.View(func(state *State) (any, error) {
		_, roots := accountTree(state)
		return map[string]any{"accounts": roots}, nil
	})
}

// GetAccount returns one account with its subtree.
func (s *Service) GetAccount(id string) (any, error) {
	return s.store.View(func(state *State) (any, error) {
		nodes, _ := accountTree(state)
		node, found := nodes[id]
		if !found {
			return nil, NotFoundError("account %s was not found", id)
		}
		return node, nil
	})
}

// GetBalance derives the account balance at asOf from the journals only.
func (s *Service) GetBalance(id, asOf string) (any, error) {
	if id == "" {
		return nil, ValidationError("account id is required")
	}
	if asOf == "" {
		return nil, ValidationError("as_of query parameter is required")
	}
	date, err := validateDate(asOf, "as_of")
	if err != nil {
		return nil, err
	}
	return s.store.View(func(state *State) (any, error) {
		account, found := state.Accounts[id]
		if !found {
			return nil, NotFoundError("account %s was not found", id)
		}
		return s.computeBalance(state, account, date), nil
	})
}

func (s *Service) computeBalance(state *State, account *Account, asOf string) *Balance {
	balance := &Balance{
		AccountID:          account.ID,
		AccountName:        account.Name,
		AccountType:        account.Type,
		NormalBalance:      account.NormalBalance,
		Currency:           account.Currency,
		FunctionalCurrency: s.functionalCurrency,
		AsOf:               asOf,
	}
	for _, journal := range sortedJournals(state) {
		if journal.Date > asOf {
			continue
		}
		for _, line := range journal.Lines {
			if line.AccountID != account.ID {
				continue
			}
			balance.PostingCount++
			if line.Side == "debit" {
				balance.DebitMinor += line.AmountMinor
				balance.FunctionalDebitMinor += line.FunctionalAmountMinor
			} else {
				balance.CreditMinor += line.AmountMinor
				balance.FunctionalCreditMinor += line.FunctionalAmountMinor
			}
			if balance.FirstPostingDate == "" || journal.Date < balance.FirstPostingDate {
				balance.FirstPostingDate = journal.Date
			}
			if journal.Date > balance.LastPostingDate {
				balance.LastPostingDate = journal.Date
			}
		}
	}
	balance.NetMinor = balance.DebitMinor - balance.CreditMinor
	balance.FunctionalNetMinor = balance.FunctionalDebitMinor - balance.FunctionalCreditMinor
	balance.BalanceMinor = balance.NetMinor
	if account.NormalBalance == "credit" {
		balance.BalanceMinor = -balance.NetMinor
	}
	return balance
}

type createRateRequest struct {
	Base  string `json:"base"`
	Quote string `json:"quote"`
	Date  string `json:"date"`
	Rate  string `json:"rate"`
}

// CreateRate stores one exchange rate snapshot.
func (s *Service) CreateRate(body []byte, key string) (any, error) {
	var request createRateRequest
	if err := decodeObject(body, &request); err != nil {
		return nil, err
	}
	base, err := validateCurrency(request.Base, "base", true)
	if err != nil {
		return nil, err
	}
	quote, err := validateCurrency(request.Quote, "quote", true)
	if err != nil {
		return nil, err
	}
	if base == quote {
		return nil, ValidationError("base and quote must be different currencies")
	}
	date, err := validateDate(request.Date, "date")
	if err != nil {
		return nil, err
	}
	numerator, denominator, err := parseRateFraction(request.Rate, "rate")
	if err != nil {
		return nil, err
	}
	return s.runIdempotent(key, fmt.Sprintf("create-rate:%s/%s/%s", base, quote, date), func(state *State) (any, error) {
		for _, existing := range state.Rates {
			if existing.Base == base && existing.Quote == quote && existing.Date == date {
				return nil, ConflictError("a %s/%s rate for %s already exists", base, quote, date)
			}
		}
		rate := &Rate{
			Base:        base,
			Quote:       quote,
			Date:        date,
			Rate:        request.Rate,
			Numerator:   numerator,
			Denominator: denominator,
			CreatedAt:   s.now(),
		}
		state.Rates = append(state.Rates, rate)
		return rate, nil
	})
}

// ListRates returns every rate snapshot in deterministic order.
func (s *Service) ListRates() (any, error) {
	return s.store.View(func(state *State) (any, error) {
		return map[string]any{"rates": sortedRates(state)}, nil
	})
}

type journalLineRequest struct {
	AccountID   string `json:"account_id"`
	Side        string `json:"side"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Rate        string `json:"rate"`
	Reference   string `json:"reference"`
}

type createJournalRequest struct {
	ID    string                `json:"id"`
	Date  string                `json:"date"`
	Memo  string                `json:"memo"`
	Lines []*journalLineRequest `json:"lines"`
}

// CreateJournal posts an immutable voucher once it balances in the functional
// currency.
func (s *Service) CreateJournal(body []byte, key string) (any, error) {
	var request createJournalRequest
	if err := decodeObject(body, &request); err != nil {
		return nil, err
	}
	id, err := validateIdentifier(request.ID, "journal id")
	if err != nil {
		return nil, err
	}
	date, err := validateDate(request.Date, "date")
	if err != nil {
		return nil, err
	}
	memo, err := validateText(request.Memo, "memo")
	if err != nil {
		return nil, err
	}
	if len(request.Lines) < 2 {
		return nil, ValidationError("lines must contain at least two postings")
	}
	return s.runIdempotent(key, "create-journal:"+id, func(state *State) (any, error) {
		if _, found := state.Journals[id]; found {
			return nil, ConflictError("journal %s already exists", id)
		}
		journal := &Journal{
			ID:                 id,
			Date:               date,
			Memo:               memo,
			FunctionalCurrency: s.functionalCurrency,
			Lines:              []*JournalLine{},
			CreatedAt:          s.now(),
		}
		var debits, credits int64
		for position, raw := range request.Lines {
			line, err := s.buildJournalLine(state, raw, position+1, date)
			if err != nil {
				return nil, err
			}
			if line.Side == "debit" {
				debits += line.FunctionalAmountMinor
			} else {
				credits += line.FunctionalAmountMinor
			}
			journal.Lines = append(journal.Lines, line)
		}
		if debits != credits {
			return nil, ValidationError(
				"journal is not balanced in %s: debits %d, credits %d",
				s.functionalCurrency, debits, credits,
			)
		}
		journal.DebitFunctionalMinor = debits
		journal.CreditFunctionalMinor = credits
		state.Journals[id] = journal
		return journal, nil
	})
}

// GetJournal returns one stored voucher.
func (s *Service) GetJournal(id string) (any, error) {
	return s.store.View(func(state *State) (any, error) {
		journal, found := state.Journals[id]
		if !found {
			return nil, NotFoundError("journal %s was not found", id)
		}
		return journal, nil
	})
}

func (s *Service) buildJournalLine(state *State, raw *journalLineRequest, index int, date string) (*JournalLine, error) {
	if raw == nil {
		return nil, ValidationError("lines[%d] must be an object", index)
	}
	accountID, err := validateIdentifier(raw.AccountID, fmt.Sprintf("lines[%d].account_id", index))
	if err != nil {
		return nil, err
	}
	account, found := state.Accounts[accountID]
	if !found {
		return nil, ValidationError("lines[%d].account_id %s does not exist", index, accountID)
	}
	if raw.Side != "debit" && raw.Side != "credit" {
		return nil, ValidationError("lines[%d].side must be debit or credit", index)
	}
	amount, err := validateAmount(raw.AmountMinor, fmt.Sprintf("lines[%d].amount_minor", index), false, false)
	if err != nil {
		return nil, err
	}
	currency, err := validateCurrency(raw.Currency, fmt.Sprintf("lines[%d].currency", index), false)
	if err != nil {
		return nil, err
	}
	if currency == "" {
		currency = account.Currency
	}
	if currency != account.Currency {
		return nil, ValidationError(
			"lines[%d].currency %s must match the currency %s of account %s",
			index, currency, account.Currency, accountID,
		)
	}
	reference, err := validateText(raw.Reference, fmt.Sprintf("lines[%d].reference", index))
	if err != nil {
		return nil, err
	}
	numerator, denominator, source, err := s.resolveRate(state, currency, date, raw.Rate, fmt.Sprintf("lines[%d].rate", index))
	if err != nil {
		return nil, err
	}
	functional, err := convertMinor(amount, numerator, denominator)
	if err != nil {
		return nil, err
	}
	if functional < 1 {
		return nil, ValidationError(
			"lines[%d] converts to zero minor units in %s; use a larger amount or a finer exchange rate",
			index, s.functionalCurrency,
		)
	}
	return &JournalLine{
		Index:                 index,
		AccountID:             accountID,
		Side:                  raw.Side,
		AmountMinor:           amount,
		Currency:              currency,
		Rate:                  raw.Rate,
		RateNumerator:         numerator,
		RateDenominator:       denominator,
		RateSource:            source,
		FunctionalAmountMinor: functional,
		Reference:             reference,
	}, nil
}

// resolveRate returns the exact fraction that converts one minor unit of
// currency into the functional currency for the given date.
func (s *Service) resolveRate(state *State, currency, date, declared, field string) (int64, int64, string, error) {
	if currency == s.functionalCurrency {
		if declared == "" || declared == "1" {
			return 1, 1, "identity", nil
		}
		numerator, denominator, err := parseRateFraction(declared, field)
		if err != nil {
			return 0, 0, "", err
		}
		if numerator != denominator {
			return 0, 0, "", ValidationError(
				"%s must be omitted or exactly 1 because %s is the functional currency",
				field, currency,
			)
		}
		return 1, 1, "identity", nil
	}
	if declared != "" {
		numerator, denominator, err := parseRateFraction(declared, field)
		if err != nil {
			return 0, 0, "", err
		}
		return numerator, denominator, "declared", nil
	}
	if direct := latestRate(state, currency, s.functionalCurrency, date); direct != nil {
		return direct.Numerator, direct.Denominator, "snapshot:" + direct.Date, nil
	}
	if inverse := latestRate(state, s.functionalCurrency, currency, date); inverse != nil {
		return inverse.Denominator, inverse.Numerator, "snapshot-inverse:" + inverse.Date, nil
	}
	return 0, 0, "", ValidationError(
		"no %s/%s exchange rate snapshot exists on or before %s",
		currency, s.functionalCurrency, date,
	)
}

type periodRequest struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type statementLineRequest struct {
	ID          string `json:"id"`
	Date        string `json:"date"`
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference"`
	Description string `json:"description"`
}

type createStatementRequest struct {
	ID                  string                  `json:"id"`
	AccountID           string                  `json:"account_id"`
	Currency            string                  `json:"currency"`
	Period              periodRequest           `json:"period"`
	OpeningBalanceMinor int64                   `json:"opening_balance_minor"`
	ClosingBalanceMinor int64                   `json:"closing_balance_minor"`
	Lines               []*statementLineRequest `json:"lines"`
}

// CreateStatement imports one bank statement.
func (s *Service) CreateStatement(body []byte, key string) (any, error) {
	var request createStatementRequest
	if err := decodeObject(body, &request); err != nil {
		return nil, err
	}
	id, err := validateIdentifier(request.ID, "statement id")
	if err != nil {
		return nil, err
	}
	accountID, err := validateIdentifier(request.AccountID, "account_id")
	if err != nil {
		return nil, err
	}
	currency, err := validateCurrency(request.Currency, "currency", false)
	if err != nil {
		return nil, err
	}
	start, err := validateDate(request.Period.Start, "period.start")
	if err != nil {
		return nil, err
	}
	end, err := validateDate(request.Period.End, "period.end")
	if err != nil {
		return nil, err
	}
	if start > end {
		return nil, ValidationError("period.start must not be after period.end")
	}
	opening, err := validateAmount(request.OpeningBalanceMinor, "opening_balance_minor", true, true)
	if err != nil {
		return nil, err
	}
	closing, err := validateAmount(request.ClosingBalanceMinor, "closing_balance_minor", true, true)
	if err != nil {
		return nil, err
	}
	return s.runIdempotent(key, "create-statement:"+id, func(state *State) (any, error) {
		if _, found := state.Statements[id]; found {
			return nil, ConflictError("statement %s already exists", id)
		}
		account, found := state.Accounts[accountID]
		if !found {
			return nil, ValidationError("account_id %s does not exist", accountID)
		}
		if currency == "" {
			currency = account.Currency
		}
		if currency != account.Currency {
			return nil, ValidationError(
				"currency %s must match the currency %s of account %s",
				currency, account.Currency, accountID,
			)
		}
		statement := &Statement{
			ID:                  id,
			AccountID:           accountID,
			Currency:            currency,
			Period:              Period{Start: start, End: end},
			OpeningBalanceMinor: opening,
			ClosingBalanceMinor: closing,
			Lines:               []*StatementLine{},
			CreatedAt:           s.now(),
		}
		total := opening
		seen := map[string]bool{}
		for position, raw := range request.Lines {
			if raw == nil {
				return nil, ValidationError("lines[%d] must be an object", position+1)
			}
			field := fmt.Sprintf("lines[%d]", position+1)
			lineID, err := validateIdentifier(raw.ID, field+".id")
			if err != nil {
				return nil, err
			}
			if seen[lineID] {
				return nil, ValidationError("statement line id %s is duplicated", lineID)
			}
			seen[lineID] = true
			lineDate, err := validateDate(raw.Date, field+".date")
			if err != nil {
				return nil, err
			}
			if lineDate < start || lineDate > end {
				return nil, ValidationError("%s.date must fall inside the statement period", field)
			}
			amount, err := validateAmount(raw.AmountMinor, field+".amount_minor", false, true)
			if err != nil {
				return nil, err
			}
			reference, err := validateText(raw.Reference, field+".reference")
			if err != nil {
				return nil, err
			}
			description, err := validateText(raw.Description, field+".description")
			if err != nil {
				return nil, err
			}
			total += amount
			statement.Lines = append(statement.Lines, &StatementLine{
				ID:          lineID,
				Date:        lineDate,
				AmountMinor: amount,
				Reference:   reference,
				Description: description,
			})
		}
		if total != closing {
			return nil, ValidationError(
				"statement lines do not reconcile with the closing balance: opening %d plus lines %d is %d, not %d",
				opening, total-opening, total, closing,
			)
		}
		state.Statements[id] = statement
		return statement, nil
	})
}

// GetStatement returns one imported statement.
func (s *Service) GetStatement(id string) (any, error) {
	return s.store.View(func(state *State) (any, error) {
		statement, found := state.Statements[id]
		if !found {
			return nil, NotFoundError("statement %s was not found", id)
		}
		return statement, nil
	})
}

type createReconciliationRequest struct {
	ID          string `json:"id"`
	StatementID string `json:"statement_id"`
}

// CreateReconciliation matches one statement against the ledger activity of its
// account and freezes the result.
func (s *Service) CreateReconciliation(body []byte, key string) (any, error) {
	var request createReconciliationRequest
	if err := decodeObject(body, &request); err != nil {
		return nil, err
	}
	id, err := validateIdentifier(request.ID, "reconciliation id")
	if err != nil {
		return nil, err
	}
	statementID, err := validateIdentifier(request.StatementID, "statement_id")
	if err != nil {
		return nil, err
	}
	return s.runIdempotent(key, "create-reconciliation:"+id, func(state *State) (any, error) {
		if _, found := state.Reconciliations[id]; found {
			return nil, ConflictError("reconciliation %s already exists", id)
		}
		statement, found := state.Statements[statementID]
		if !found {
			return nil, NotFoundError("statement %s was not found", statementID)
		}
		reconciliation, err := s.computeReconciliation(state, statement, id, s.now())
		if err != nil {
			return nil, err
		}
		state.Reconciliations[id] = reconciliation
		return reconciliation, nil
	})
}

// GetReconciliation returns one stored reconciliation.
func (s *Service) GetReconciliation(id string) (any, error) {
	return s.store.View(func(state *State) (any, error) {
		reconciliation, found := state.Reconciliations[id]
		if !found {
			return nil, NotFoundError("reconciliation %s was not found", id)
		}
		return reconciliation, nil
	})
}

// ledgerEntry is one journal line seen from the ledger side of a
// reconciliation. AmountMinor is signed with debits positive.
type ledgerEntry struct {
	ID          string
	Date        string
	AmountMinor int64
	Reference   string
}

func ledgerEntries(state *State, accountID string, period Period) []*ledgerEntry {
	entries := []*ledgerEntry{}
	for _, journal := range sortedJournals(state) {
		if journal.Date < period.Start || journal.Date > period.End {
			continue
		}
		for _, line := range journal.Lines {
			if line.AccountID != accountID {
				continue
			}
			amount := line.AmountMinor
			if line.Side == "credit" {
				amount = -amount
			}
			entries = append(entries, &ledgerEntry{
				ID:          fmt.Sprintf("%s#%d", journal.ID, line.Index),
				Date:        journal.Date,
				AmountMinor: amount,
				Reference:   line.Reference,
			})
		}
	}
	return entries
}

var differenceOrder = map[string]int{
	"timing":               0,
	"amount_mismatch":      1,
	"missing_in_ledger":    2,
	"missing_in_statement": 3,
}

// computeReconciliation matches statement lines to ledger lines in three
// deterministic tiers and classifies everything that stays unmatched.
func (s *Service) computeReconciliation(state *State, statement *Statement, id, createdAt string) (*Reconciliation, error) {
	lines := make([]*StatementLine, 0, len(statement.Lines))
	lines = append(lines, statement.Lines...)
	sort.Slice(lines, func(left, right int) bool { return lines[left].ID < lines[right].ID })

	entries := ledgerEntries(state, statement.AccountID, statement.Period)
	sort.Slice(entries, func(left, right int) bool { return entries[left].ID < entries[right].ID })

	statementUsed := make([]bool, len(lines))
	ledgerUsed := make([]bool, len(entries))
	differences := []*Difference{}
	matched := 0
	match := func(statementIndex, ledgerIndex int, difference *Difference) {
		statementUsed[statementIndex] = true
		ledgerUsed[ledgerIndex] = true
		matched++
		if difference != nil {
			differences = append(differences, difference)
		}
	}

	// Tier 1: same signed amount and same date.
	for index, line := range lines {
		for other, entry := range entries {
			if ledgerUsed[other] {
				continue
			}
			if entry.AmountMinor == line.AmountMinor && entry.Date == line.Date {
				match(index, other, nil)
				break
			}
		}
	}

	// Tier 2: same signed amount, different date, so it only differs in timing.
	for index, line := range lines {
		if statementUsed[index] {
			continue
		}
		for other, entry := range entries {
			if ledgerUsed[other] || entry.AmountMinor != line.AmountMinor {
				continue
			}
			statementAmount := line.AmountMinor
			ledgerAmount := entry.AmountMinor
			match(index, other, &Difference{
				Type:                 "timing",
				Detail:               "the amounts agree but the ledger date differs from the statement date",
				StatementLineID:      line.ID,
				LedgerLineID:         entry.ID,
				StatementDate:        line.Date,
				LedgerDate:           entry.Date,
				StatementAmountMinor: &statementAmount,
				LedgerAmountMinor:    &ledgerAmount,
			})
			break
		}
	}

	// Tier 3: same non-empty reference but a different amount.
	for index, line := range lines {
		if statementUsed[index] || line.Reference == "" {
			continue
		}
		for other, entry := range entries {
			if ledgerUsed[other] || entry.Reference == "" || entry.Reference != line.Reference {
				continue
			}
			statementAmount := line.AmountMinor
			ledgerAmount := entry.AmountMinor
			match(index, other, &Difference{
				Type:                 "amount_mismatch",
				Detail:               fmt.Sprintf("reference %s appears on both sides with different amounts", line.Reference),
				StatementLineID:      line.ID,
				LedgerLineID:         entry.ID,
				StatementDate:        line.Date,
				LedgerDate:           entry.Date,
				StatementAmountMinor: &statementAmount,
				LedgerAmountMinor:    &ledgerAmount,
				DifferenceMinor:      statementAmount - ledgerAmount,
			})
			break
		}
	}

	// Everything still unmatched is missing on one side.
	for index, line := range lines {
		if statementUsed[index] {
			continue
		}
		amount := line.AmountMinor
		differences = append(differences, &Difference{
			Type:                 "missing_in_ledger",
			Detail:               "the statement line has no journal line in the period",
			StatementLineID:      line.ID,
			StatementDate:        line.Date,
			StatementAmountMinor: &amount,
			DifferenceMinor:      amount,
		})
	}
	for index, entry := range entries {
		if ledgerUsed[index] {
			continue
		}
		amount := entry.AmountMinor
		differences = append(differences, &Difference{
			Type:              "missing_in_statement",
			Detail:            "the journal line has no statement line in the period",
			LedgerLineID:      entry.ID,
			LedgerDate:        entry.Date,
			LedgerAmountMinor: &amount,
			DifferenceMinor:   -amount,
		})
	}

	sort.SliceStable(differences, func(left, right int) bool {
		a, b := differences[left], differences[right]
		if differenceOrder[a.Type] != differenceOrder[b.Type] {
			return differenceOrder[a.Type] < differenceOrder[b.Type]
		}
		if a.StatementLineID != b.StatementLineID {
			return a.StatementLineID < b.StatementLineID
		}
		return a.LedgerLineID < b.LedgerLineID
	})

	var ledgerNet int64
	for _, entry := range entries {
		ledgerNet += entry.AmountMinor
	}
	statementNet := statement.ClosingBalanceMinor - statement.OpeningBalanceMinor

	summary := DifferenceSummary{}
	var explained int64
	for _, difference := range differences {
		explained += difference.DifferenceMinor
		switch difference.Type {
		case "timing":
			summary.Timing++
		case "amount_mismatch":
			summary.AmountMismatch++
		case "missing_in_ledger":
			summary.MissingInLedger++
		case "missing_in_statement":
			summary.MissingInStatement++
		default:
			return nil, InternalError("reconciliation produced the unknown difference type %s", difference.Type)
		}
	}
	if 2*matched+summary.MissingInLedger+summary.MissingInStatement != len(lines)+len(entries) {
		return nil, InternalError("reconciliation differences are not mutually exclusive and complete")
	}
	if explained != statementNet-ledgerNet {
		return nil, InternalError("reconciliation differences do not add up to the period difference")
	}

	status := "balanced"
	if len(differences) > 0 {
		status = "differences_found"
	}
	return &Reconciliation{
		ID:                 id,
		StatementID:        statement.ID,
		AccountID:          statement.AccountID,
		Currency:           statement.Currency,
		FunctionalCurrency: s.functionalCurrency,
		Period:             statement.Period,
		Status:             status,
		MatchedCount:       matched,
		StatementLineCount: len(lines),
		LedgerLineCount:    len(entries),
		StatementNetMinor:  statementNet,
		LedgerNetMinor:     ledgerNet,
		DifferenceMinor:    statementNet - ledgerNet,
		Summary:            summary,
		Differences:        differences,
		CreatedAt:          createdAt,
	}, nil
}

func sortedAccounts(state *State) []*Account {
	accounts := make([]*Account, 0, len(state.Accounts))
	for _, account := range state.Accounts {
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(left, right int) bool { return accounts[left].ID < accounts[right].ID })
	return accounts
}

func sortedJournals(state *State) []*Journal {
	journals := make([]*Journal, 0, len(state.Journals))
	for _, journal := range state.Journals {
		journals = append(journals, journal)
	}
	sort.Slice(journals, func(left, right int) bool {
		if journals[left].Date != journals[right].Date {
			return journals[left].Date < journals[right].Date
		}
		return journals[left].ID < journals[right].ID
	})
	return journals
}

func sortedRates(state *State) []*Rate {
	rates := make([]*Rate, 0, len(state.Rates))
	rates = append(rates, state.Rates...)
	sort.Slice(rates, func(left, right int) bool {
		a, b := rates[left], rates[right]
		if a.Base != b.Base {
			return a.Base < b.Base
		}
		if a.Quote != b.Quote {
			return a.Quote < b.Quote
		}
		return a.Date < b.Date
	})
	return rates
}

func accountTree(state *State) (map[string]*AccountNode, []*AccountNode) {
	nodes := map[string]*AccountNode{}
	for _, account := range state.Accounts {
		nodes[account.ID] = &AccountNode{Account: *account, Children: []*AccountNode{}}
	}
	roots := []*AccountNode{}
	for _, account := range sortedAccounts(state) {
		node := nodes[account.ID]
		if account.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, found := nodes[*account.ParentID]
		if !found {
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	return nodes, roots
}

// latestRate returns the most recent snapshot for one direction that is
// effective on the given date.
func latestRate(state *State, base, quote, asOf string) *Rate {
	var best *Rate
	for _, rate := range state.Rates {
		if rate.Base != base || rate.Quote != quote || rate.Date > asOf {
			continue
		}
		if best == nil || rate.Date > best.Date {
			best = rate
		}
	}
	return best
}
