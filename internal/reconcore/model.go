package reconcore

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"
)

const (
	// dateLayout is the only accepted date format.
	dateLayout = "2006-01-02"

	// maxAmountMinor bounds every monetary value so that conversions stay
	// exactly representable in int64 arithmetic.
	maxAmountMinor = int64(1_000_000_000_000_000)

	maxIdentifierLength = 128
	maxTextLength       = 256
	maxRateDigits       = 18
	maxRateDecimals     = 9
)

// normalBalanceByType maps an account type onto the side that increases it.
var normalBalanceByType = map[string]string{
	"asset":     "debit",
	"expense":   "debit",
	"liability": "credit",
	"equity":    "credit",
	"revenue":   "credit",
}

// Rate is an immutable exchange rate snapshot: one unit of Base is worth Rate
// units of Quote from Date onwards. The decimal string is stored together with
// its exact reduced fraction so nothing is ever recomputed from a float.
type Rate struct {
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	Date        string `json:"date"`
	Rate        string `json:"rate"`
	Numerator   int64  `json:"numerator"`
	Denominator int64  `json:"denominator"`
	CreatedAt   string `json:"created_at"`
}

// IdempotencyRecord stores the first response produced for a key.
type IdempotencyRecord struct {
	Key       string `json:"key"`
	Operation string `json:"operation"`
	Response  string `json:"response"`
	CreatedAt string `json:"created_at"`
}

// Account is one node of the chart of accounts. Every account holds exactly one
// currency.
type Account struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	ParentID      *string `json:"parent_id"`
	Currency      string  `json:"currency"`
	NormalBalance string  `json:"normal_balance"`
	CreatedAt     string  `json:"created_at"`
}

// AccountNode is an Account with its subtree attached.
type AccountNode struct {
	Account
	Children []*AccountNode `json:"children"`
}

// JournalLine is one balanced-verified posting. AmountMinor is expressed in the
// line currency; FunctionalAmountMinor is the same amount converted into the
// functional currency with the resolved rate.
type JournalLine struct {
	Index                 int    `json:"index"`
	AccountID             string `json:"account_id"`
	Side                  string `json:"side"`
	AmountMinor           int64  `json:"amount_minor"`
	Currency              string `json:"currency"`
	Rate                  string `json:"rate"`
	RateNumerator         int64  `json:"rate_numerator"`
	RateDenominator       int64  `json:"rate_denominator"`
	RateSource            string `json:"rate_source"`
	FunctionalAmountMinor int64  `json:"functional_amount_minor"`
	Reference             string `json:"reference,omitempty"`
}

// Journal is an immutable, balanced voucher. ReversalOf is set only on a
// reversal journal and names the journal it cancels; ordinary journals never
// carry the field.
type Journal struct {
	ID                    string         `json:"id"`
	Date                  string         `json:"date"`
	Memo                  string         `json:"memo,omitempty"`
	FunctionalCurrency    string         `json:"functional_currency"`
	Lines                 []*JournalLine `json:"lines"`
	DebitFunctionalMinor  int64          `json:"debit_functional_minor"`
	CreditFunctionalMinor int64          `json:"credit_functional_minor"`
	CreatedAt             string         `json:"created_at"`
	ReversalOf            string         `json:"reversal_of,omitempty"`
}

// Period is an inclusive date window.
type Period struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// PeriodClose freezes one calendar month. Once a close exists, ordinary
// journals dated inside the period are rejected and only adjustment journals
// posted through the close itself are appended to it.
type PeriodClose struct {
	ID                   string   `json:"id"`
	Period               Period   `json:"period"`
	Status               string   `json:"status"`
	ClosedAt             string   `json:"closed_at"`
	AdjustmentCount      int      `json:"adjustment_count"`
	AdjustmentJournalIDs []string `json:"adjustment_journal_ids"`
}

// StatementLine is one bank statement line. AmountMinor is signed: positive
// means money into the account (a debit to it), negative means money out.
type StatementLine struct {
	ID          string `json:"id"`
	Date        string `json:"date"`
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference,omitempty"`
	Description string `json:"description,omitempty"`
}

// Statement is an imported bank statement for one account.
type Statement struct {
	ID                  string           `json:"id"`
	AccountID           string           `json:"account_id"`
	Currency            string           `json:"currency"`
	Period              Period           `json:"period"`
	OpeningBalanceMinor int64            `json:"opening_balance_minor"`
	ClosingBalanceMinor int64            `json:"closing_balance_minor"`
	Lines               []*StatementLine `json:"lines"`
	CreatedAt           string           `json:"created_at"`
}

// Difference is one classified reconciliation difference. Exactly one statement
// line and/or ledger line is referenced, never both buckets for the same line.
type Difference struct {
	Type                 string `json:"type"`
	Detail               string `json:"detail"`
	StatementLineID      string `json:"statement_line_id,omitempty"`
	LedgerLineID         string `json:"ledger_line_id,omitempty"`
	StatementDate        string `json:"statement_date,omitempty"`
	LedgerDate           string `json:"ledger_date,omitempty"`
	StatementAmountMinor *int64 `json:"statement_amount_minor,omitempty"`
	LedgerAmountMinor    *int64 `json:"ledger_amount_minor,omitempty"`
	DifferenceMinor      int64  `json:"difference_minor"`
}

// DifferenceSummary counts the four mutually exclusive difference categories.
type DifferenceSummary struct {
	Timing             int `json:"timing"`
	AmountMismatch     int `json:"amount_mismatch"`
	MissingInLedger    int `json:"missing_in_ledger"`
	MissingInStatement int `json:"missing_in_statement"`
}

// Reconciliation is the frozen outcome of matching one statement against the
// ledger activity of its account inside its period.
type Reconciliation struct {
	ID                 string            `json:"id"`
	StatementID        string            `json:"statement_id"`
	AccountID          string            `json:"account_id"`
	Currency           string            `json:"currency"`
	FunctionalCurrency string            `json:"functional_currency"`
	Period             Period            `json:"period"`
	Status             string            `json:"status"`
	MatchedCount       int               `json:"matched_count"`
	StatementLineCount int               `json:"statement_line_count"`
	LedgerLineCount    int               `json:"ledger_line_count"`
	StatementNetMinor  int64             `json:"statement_net_minor"`
	LedgerNetMinor     int64             `json:"ledger_net_minor"`
	DifferenceMinor    int64             `json:"difference_minor"`
	Summary            DifferenceSummary `json:"summary"`
	Differences        []*Difference     `json:"differences"`
	CreatedAt          string            `json:"created_at"`
}

// Balance is the derived position of one account at one date.
type Balance struct {
	AccountID             string `json:"account_id"`
	AccountName           string `json:"account_name"`
	AccountType           string `json:"account_type"`
	NormalBalance         string `json:"normal_balance"`
	Currency              string `json:"currency"`
	FunctionalCurrency    string `json:"functional_currency"`
	AsOf                  string `json:"as_of"`
	DebitMinor            int64  `json:"debit_minor"`
	CreditMinor           int64  `json:"credit_minor"`
	NetMinor              int64  `json:"net_minor"`
	BalanceMinor          int64  `json:"balance_minor"`
	FunctionalDebitMinor  int64  `json:"functional_debit_minor"`
	FunctionalCreditMinor int64  `json:"functional_credit_minor"`
	FunctionalNetMinor    int64  `json:"functional_net_minor"`
	PostingCount          int    `json:"posting_count"`
	FirstPostingDate      string `json:"first_posting_date,omitempty"`
	LastPostingDate       string `json:"last_posting_date,omitempty"`
}

// TrialBalanceRow is one account's line on the trial balance report. Only
// journal lines posted directly to the account are summed; child accounts
// never roll up into their parent.
type TrialBalanceRow struct {
	AccountID             string `json:"account_id"`
	AccountName           string `json:"account_name"`
	Currency              string `json:"currency"`
	DebitMinor            int64  `json:"debit_minor"`
	CreditMinor           int64  `json:"credit_minor"`
	NetMinor              int64  `json:"net_minor"`
	FunctionalDebitMinor  int64  `json:"functional_debit_minor"`
	FunctionalCreditMinor int64  `json:"functional_credit_minor"`
	FunctionalNetMinor    int64  `json:"functional_net_minor"`
	PostingCount          int    `json:"posting_count"`
}

// TrialBalance is the read-only as-of trial balance. The debit total collects
// every row with a non-negative functional net, the credit total collects the
// absolute value of every negative row.
type TrialBalance struct {
	AsOf                       string             `json:"as_of"`
	FunctionalCurrency         string             `json:"functional_currency"`
	Accounts                   []*TrialBalanceRow `json:"accounts"`
	FunctionalDebitTotalMinor  int64              `json:"functional_debit_total_minor"`
	FunctionalCreditTotalMinor int64              `json:"functional_credit_total_minor"`
	PostingCount               int                `json:"posting_count"`
	Status                     string             `json:"status"`
}

// FinancialStatementAccount is one account row on a financial statement. The
// balance is the stored functional-currency total oriented by the account type
// (assets and expenses are debit minus credit, the rest credit minus debit);
// child accounts never roll up into their parent.
type FinancialStatementAccount struct {
	AccountID              string  `json:"account_id"`
	AccountName            string  `json:"account_name"`
	ParentID               *string `json:"parent_id"`
	Currency               string  `json:"currency"`
	FunctionalBalanceMinor int64   `json:"functional_balance_minor"`
}

// BalanceSheet is the as-of section of the financial statements. Equity total
// includes the income earned through as_of so the balance check reproduces the
// accounting equation assets = liabilities + equity.
type BalanceSheet struct {
	Assets                []*FinancialStatementAccount `json:"assets"`
	Liabilities           []*FinancialStatementAccount `json:"liabilities"`
	Equity                []*FinancialStatementAccount `json:"equity"`
	AssetsTotalMinor      int64                        `json:"assets_total_minor"`
	LiabilitiesTotalMinor int64                        `json:"liabilities_total_minor"`
	EquityTotalMinor      int64                        `json:"equity_total_minor"`
	BalanceCheckMinor     int64                        `json:"balance_check_minor"`
	Status                string                       `json:"status"`
}

// IncomeStatement is the closed-period section of the financial statements.
// Net income uses only the journals dated inside period_start..period_end.
type IncomeStatement struct {
	Revenue            []*FinancialStatementAccount `json:"revenue"`
	Expenses           []*FinancialStatementAccount `json:"expenses"`
	RevenueTotalMinor  int64                        `json:"revenue_total_minor"`
	ExpensesTotalMinor int64                        `json:"expenses_total_minor"`
	NetIncomeMinor     int64                        `json:"net_income_minor"`
}

// FinancialStatements is the read-only month-end report. It derives a balance
// sheet at as_of and an income statement for the closed period, recomputed from
// the stored journals on every call and never writing state.
type FinancialStatements struct {
	AsOf               string           `json:"as_of"`
	PeriodStart        string           `json:"period_start"`
	PeriodEnd          string           `json:"period_end"`
	FunctionalCurrency string           `json:"functional_currency"`
	BalanceSheet       *BalanceSheet    `json:"balance_sheet"`
	IncomeStatement    *IncomeStatement `json:"income_statement"`
}

// decodeObject decodes exactly one JSON object and rejects unknown fields,
// trailing content and non-object bodies.
func decodeObject(body []byte, target any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ValidationError("request body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return ValidationError(
			"request body does not match the documented schema: %s",
			strings.TrimPrefix(err.Error(), "json: "),
		)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ValidationError("request body must contain exactly one JSON object")
	}
	return nil
}

// encodeResponse renders a response as canonical JSON.
func encodeResponse(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", InternalError("response could not be encoded: %s", err)
	}
	return string(encoded), nil
}

// decodeResponse restores a stored response without losing integer precision.
func decodeResponse(encoded string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, InternalError("stored response could not be decoded: %s", err)
	}
	return value, nil
}

// normalizeResponse turns any response value into its canonical JSON shape so a
// replayed idempotent request returns byte-identical JSON.
func normalizeResponse(value any) (any, error) {
	encoded, err := encodeResponse(value)
	if err != nil {
		return nil, err
	}
	return decodeResponse(encoded)
}

func validateIdentifier(value, field string) (string, error) {
	if value == "" {
		return "", ValidationError("%s is required", field)
	}
	if len(value) > maxIdentifierLength {
		return "", ValidationError("%s must be at most %d characters", field, maxIdentifierLength)
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return "", ValidationError("%s must contain printable ASCII characters without whitespace", field)
		}
	}
	return value, nil
}

func validateText(value, field string) (string, error) {
	if len(value) > maxTextLength {
		return "", ValidationError("%s must be at most %d characters", field, maxTextLength)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", ValidationError("%s must not contain control characters", field)
		}
	}
	return value, nil
}

func validateDate(value, field string) (string, error) {
	parsed, err := time.Parse(dateLayout, value)
	if err != nil || parsed.Format(dateLayout) != value {
		return "", ValidationError("%s must be an ISO 8601 calendar date (YYYY-MM-DD)", field)
	}
	return value, nil
}

func validateCurrency(value, field string, required bool) (string, error) {
	if value == "" {
		if required {
			return "", ValidationError("%s is required", field)
		}
		return "", nil
	}
	if len(value) != 3 {
		return "", ValidationError("%s must be a three letter uppercase ISO 4217 currency code", field)
	}
	for index := 0; index < 3; index++ {
		if value[index] < 'A' || value[index] > 'Z' {
			return "", ValidationError("%s must be a three letter uppercase ISO 4217 currency code", field)
		}
	}
	return value, nil
}

func validateAmount(value int64, field string, allowZero, allowNegative bool) (int64, error) {
	if value > maxAmountMinor || value < -maxAmountMinor {
		return 0, ValidationError("%s must be at most %d minor units in magnitude", field, maxAmountMinor)
	}
	if value == 0 && !allowZero {
		return 0, ValidationError("%s must not be zero", field)
	}
	if value < 0 && !allowNegative {
		return 0, ValidationError("%s must not be negative", field)
	}
	return value, nil
}

// parseRateFraction converts an exact decimal string into a positive reduced
// fraction. Exponent notation, signs and more than nine decimals are rejected.
func parseRateFraction(value, field string) (int64, int64, error) {
	invalid := ValidationError("%s must be a positive decimal number such as 7.2450", field)
	if value == "" {
		return 0, 0, ValidationError("%s is required", field)
	}
	whole, fraction, _ := strings.Cut(value, ".")
	if strings.Contains(fraction, ".") {
		return 0, 0, invalid
	}
	digits := whole + fraction
	if digits == "" {
		return 0, 0, invalid
	}
	for index := 0; index < len(digits); index++ {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, 0, invalid
		}
	}
	if len(fraction) > maxRateDecimals {
		return 0, 0, ValidationError("%s must have at most %d decimal places", field, maxRateDecimals)
	}
	if len(digits) > maxRateDigits {
		return 0, 0, ValidationError("%s must have at most %d digits", field, maxRateDigits)
	}
	numerator, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, 0, invalid
	}
	if numerator <= 0 {
		return 0, 0, ValidationError("%s must be greater than zero", field)
	}
	denominator := pow10(len(fraction))
	divisor := greatestCommonDivisor(numerator, denominator)
	return numerator / divisor, denominator / divisor, nil
}

// convertMinor multiplies a minor amount by an exact fraction and rounds half
// away from zero, which is the only rounding rule ReconCore uses.
func convertMinor(amount, numerator, denominator int64) (int64, error) {
	if denominator <= 0 || numerator <= 0 {
		return 0, InternalError("exchange rate fraction is invalid")
	}
	product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(numerator))
	divisor := big.NewInt(denominator)
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(product, divisor, remainder)
	doubled := new(big.Int).Abs(remainder)
	doubled.Lsh(doubled, 1)
	if doubled.Cmp(divisor) >= 0 {
		if quotient.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	if !quotient.IsInt64() {
		return 0, ValidationError("converted amount is outside the supported range")
	}
	return quotient.Int64(), nil
}

func pow10(exponent int) int64 {
	value := int64(1)
	for index := 0; index < exponent; index++ {
		value *= 10
	}
	return value
}

func greatestCommonDivisor(left, right int64) int64 {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}
