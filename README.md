# ReconCore

ReconCore is a small multi-currency double-entry ledger and bank reconciliation
engine. It stores a chart of accounts, balanced journals, dated exchange rate
snapshots and imported bank statements, derives account balances from the
journals alone, and classifies every difference between a statement and the
ledger activity of the same account and period.

The initial release intentionally supports a compact public contract:

- every amount is an integer in the currency's minor unit, so no float ever touches money;
- a journal is stored only when its debit and credit totals agree in the functional currency;
- exchange rates come from dated snapshots, or from an explicit rate on a posting line;
- `as_of` balances are derived from journal dates, so posting later never changes an earlier answer;
- `GET /reports/trial-balance` is a read-only as-of report that sums the stored
  functional amounts of every account and never writes state;
- `GET /reports/financial-statements` is a read-only month-end report that
  builds a balance sheet at `as_of` and an income statement over a closed
  period, reuses the stored functional amounts and never writes state;
- every reconciliation difference is `timing`, `amount_mismatch`, `missing_in_ledger` or `missing_in_statement`;
- duplicate commands with the same idempotency key return the original result.

## Requirements

- Go 1.22 or newer
- no third-party modules; `go.mod` has no requirements

## Run the service

```bash
go build -o reconcore .
./reconcore --host 127.0.0.1 --port 8080 --database reconcore.db --functional-currency CNY
```

The process prints `ReconCore listening on http://127.0.0.1:8080` after it has
bound the port.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--host` | `127.0.0.1` | interface to bind |
| `--port` | `8080` | TCP port to bind |
| `--database` | `reconcore.db` | JSON database file, written atomically, created on first write |
| `--functional-currency` | `CNY` | reporting currency that every journal balances in |

## Data model

**Amounts and dates.** Every monetary value is an `int64` count of minor units
in one currency (`100000` is 1000.00 in a two-decimal currency), bounded by
`1000000000000000` in magnitude. All arithmetic is integer arithmetic. Dates are
`YYYY-MM-DD` calendar dates, periods include both ends, and date strings compare
chronologically.

**Accounts.** `id` is 1–128 printable ASCII characters without whitespace and is
unique. `type` is `asset`, `liability`, `equity`, `revenue` or `expense`;
`normal_balance` is derived from it (`asset` and `expense` are debit-normal, the
rest credit-normal). `parent_id` is `null` or an existing account id, which
builds a forest. Each account holds exactly one `currency`.

**Exchange rate snapshots.** `POST /rates` stores an immutable snapshot: one
unit of `base` equals `rate` units of `quote` from `date` onwards, where `rate`
is a plain positive decimal string with at most 9 decimals, for example
`"7.245"`. A `base`/`quote`/`date` triple can be stored only once. A journal line
in a foreign currency resolves its rate at the journal date in this order:

1. the line's own `rate`, when supplied;
2. the newest `currency`/`functional` snapshot with `date <= journal date`;
3. the newest inverse `functional`/`currency` snapshot, used as its reciprocal;
4. otherwise the request fails with `validation_error`.

The resolved fraction and its source (`identity`, `declared`,
`snapshot:<date>`, `snapshot-inverse:<date>`) are stored on the line, so later
rate snapshots never change an existing journal.

**Conversion.** `functional_amount_minor` is
`round_half_away_from_zero(amount_minor * numerator / denominator)`, computed
exactly with big integers. A line that converts to zero minor units is rejected,
because rounding must never silently drop money.

**Journals.** A journal has an `id`, a `date`, an optional `memo` and at least two
lines. Each line has `account_id`, `side` (`debit` or `credit`), a positive
`amount_minor`, an optional `currency` that must equal the account currency when
supplied, an optional `rate` and an optional `reference`. Lines are numbered
from 1 and a ledger line is identified as `"<journal id>#<index>"`. The journal
is rejected unless `Σ debit functional = Σ credit functional`; both totals are
returned as `debit_functional_minor` and `credit_functional_minor`. Journals are
immutable and posting never rewrites history. A posted journal can be undone
exactly once by a reversal journal posted through
`POST /journals/{id}/reversals`: the reversal copies every line with the debit
and credit sides exchanged, keeps the stored rate fraction, `rate_source` and
`functional_amount_minor` of each line (rates are never re-resolved at the
reversal date), and carries `reversal_of` with the original journal id, so the
two vouchers cancel exactly in both the original and the functional currency.
A reversal cannot itself be reversed, and the reversal date must not be before
the original date or inside a closed month.

**Balances.** `GET /accounts/{id}/balance?as_of=` sums the lines of that account
in journals with `date <= as_of` and returns `debit_minor`, `credit_minor`,
`net_minor` (debits minus credits), `balance_minor` (`net_minor` oriented by
`normal_balance`, so a credit-normal account grows when credited), `posting_count`,
the first and last posting date, and the same totals in the functional currency.
Journals dated after `as_of` are not read at all.

**Trial balance.** `GET /reports/trial-balance?as_of=` is read-only and is
recomputed from the stored accounts and immutable journals on every call; it
never writes state. Every account gets one row ordered by account id, even when
it has no postings, and child accounts are not rolled into their parent. Only
lines of journals with `date <= as_of` are counted. Each row carries
`account_id`, `account_name`, `currency`, the account-currency
`debit_minor`, `credit_minor` and `net_minor` (debits minus credits), the
functional totals stored on the lines (`functional_debit_minor`,
`functional_credit_minor`, `functional_net_minor`) and `posting_count`. The
footer reports `as_of`, `functional_currency`, `functional_debit_total_minor`
(sum of every non-negative `functional_net_minor`),
`functional_credit_total_minor` (sum of the absolute value of every negative
row), the sum of every row's `posting_count`, and `status`: `balanced` when the
two functional totals are equal, otherwise `unbalanced`. An empty ledger
returns an empty `accounts` array and all-zero totals.

**Financial statements.** `GET /reports/financial-statements` is read-only and
recomputed on every call; it never writes state. `as_of` cuts the balance sheet
(journals with `date <= as_of`) while `period_start` and `period_end` bound the
income statement inclusively, and `period_start <= period_end <= as_of` must
hold. Every amount is functional-currency minor units taken from the stored
`functional_amount_minor`, so no exchange rate is ever recomputed. The balance
sheet lists `assets`, `liabilities` and `equity`, and the income statement lists
`revenue` and `expenses`; within each section accounts are ordered by id, every
account gets one row even at zero, and children stay on their own row. Assets
and expenses orient debits minus credits; the other sections orient credits
minus debits. Each row carries `account_id`, `account_name`, `parent_id`,
`currency` and `functional_balance_minor`. `equity_total_minor` is equity
credit-minus-debit through `as_of` plus revenue minus expenses through `as_of`;
`balance_check_minor` is assets minus liabilities minus equity and drives
`balance_sheet.status` (`balanced` at zero, otherwise `unbalanced`).
`net_income_minor` is period revenue minus period expenses; an empty ledger
returns empty arrays, zero totals and `balanced`.

**Statements.** `POST /statements` imports one statement for one account.
`period` is `{start, end}` and every line date must fall inside it.
`opening_balance_minor + Σ line amount_minor` must equal `closing_balance_minor`.
Line amounts are signed: positive is money into the account (a debit to it),
negative is money out. Line ids must be unique inside the statement.

**Period closes.** `POST /period-closes` freezes one calendar month: `period`
must be exactly one natural month, first day to last day, and each month can be
closed only once. Once a month is closed, an ordinary journal dated
inside it is rejected with `closed period rejects journal`. Adjustments still
enter the closed month through `POST /period-closes/{id}/adjustments`, which
follows every ordinary journal rule and additionally requires a non-empty
`memo` and a `date` inside the closed period. The close records
`adjustment_count` and `adjustment_journal_ids` as adjustments are posted.

**Reconciliation.** `POST /reconciliations` matches one statement against the
ledger lines of the statement's account whose journal date falls inside the
statement period, then freezes the result. Matching is deterministic, in three
tiers over ascending ids:

1. **exact** — same signed amount and same date;
2. **timing** — same signed amount, different date;
3. **amount_mismatch** — same non-empty `reference`, different amount.

Nothing is dropped: an unmatched statement line becomes `missing_in_ledger` and
an unmatched ledger line becomes `missing_in_statement`. Every statement line and
every ledger line is either matched once or reported in exactly one difference,
and `Σ difference_minor` always equals `difference_minor`, which is
`statement_net_minor - ledger_net_minor`. A difference's `difference_minor` is
the statement side the ledger does not explain: `0` for `timing`,
`statement - ledger` for `amount_mismatch`, the statement amount for
`missing_in_ledger`, and the negated ledger amount for `missing_in_statement`.
`status` is `balanced` when there is no difference at all, otherwise
`differences_found`.

## HTTP API

Bodies are JSON. Every state-changing `POST` requires an `Idempotency-Key`
header. Unknown body fields and unknown query parameters are rejected.

### Health

```http
GET /health
```

```json
{"status":"ok"}
```

### Create an account

```http
POST /accounts
Idempotency-Key: acc-1

{"id":"1200","name":"USD cash","type":"asset","parent_id":"1000","currency":"USD"}
```

Returns HTTP 201 with the stored account:

```json
{"id":"1200","name":"USD cash","type":"asset","parent_id":"1000","currency":"USD",
 "normal_balance":"debit","created_at":"2024-06-01T12:00:00Z"}
```

### Inspect the chart of accounts

```http
GET /accounts
GET /accounts/1000
```

`GET /accounts` returns the forest of root accounts and `GET /accounts/{id}`
returns the subtree of one account. Each node is an account plus a `children`
array, siblings ordered by id:

```json
{"id":"1000","name":"Assets","type":"asset","parent_id":null,"currency":"CNY",
 "normal_balance":"debit","created_at":"2024-06-01T12:00:00Z","children":[
   {"id":"1200","name":"USD cash","type":"asset","parent_id":"1000","currency":"USD",
    "normal_balance":"debit","created_at":"2024-06-01T12:00:00Z","children":[]}]}
```

### Derive a balance

```http
GET /accounts/1200/balance?as_of=2024-01-31
```

```json
{"account_id":"1200","account_name":"USD cash","account_type":"asset",
 "normal_balance":"debit","currency":"USD","functional_currency":"CNY",
 "as_of":"2024-01-31","debit_minor":100000,"credit_minor":0,"net_minor":100000,
 "balance_minor":100000,"functional_debit_minor":724500,
 "functional_credit_minor":0,"functional_net_minor":724500,"posting_count":1,
 "first_posting_date":"2024-01-15","last_posting_date":"2024-01-15"}
```

### Read the trial balance

```http
GET /reports/trial-balance?as_of=2024-01-31
```

A read-only report over every account. Accounts are ordered by id, child
accounts stay on their own row, and only journals dated on or before `as_of`
count. A missing, malformed or repeated `as_of`, or any unknown query
parameter, returns HTTP 400 with `validation_error`.

```json
{"as_of":"2024-01-31","functional_currency":"CNY","accounts":[
  {"account_id":"1200","account_name":"USD cash","currency":"USD",
   "debit_minor":100000,"credit_minor":0,"net_minor":100000,
   "functional_debit_minor":724500,"functional_credit_minor":0,
   "functional_net_minor":724500,"posting_count":1},
  {"account_id":"4200","account_name":"USD revenue","currency":"USD",
   "debit_minor":0,"credit_minor":100000,"net_minor":-100000,
   "functional_debit_minor":0,"functional_credit_minor":724500,
   "functional_net_minor":-724500,"posting_count":1}],
 "functional_debit_total_minor":724500,
 "functional_credit_total_minor":724500,
 "posting_count":2,"status":"balanced"}
```

### Read the month-end financial statements

```http
GET /reports/financial-statements?as_of=2024-01-31&period_start=2024-01-01&period_end=2024-01-31
```

A read-only report that returns a `balance_sheet` dated at `as_of` and an
`income_statement` over the inclusive period `period_start`..`period_end`. The
three date parameters must each appear exactly once, be `YYYY-MM-DD` dates and
satisfy `period_start <= period_end <= as_of`; a missing, malformed, repeated
or out-of-order date, or any unknown query parameter, returns HTTP 400 with
`validation_error`, reporting the first problem in `as_of`, `period_start`,
`period_end` order. The report writes no snapshot, creates no voucher and
changes no close.

Every amount is expressed in the functional currency's minor unit and comes
from the `functional_amount_minor` already stored on each journal line — rates
are never recomputed. Each section lists the accounts of its type ordered by
account id, one row per account even with no postings (`functional_balance_minor`
is then zero), and child accounts are never rolled into their parent. Each row
carries `account_id`, `account_name`, `parent_id`, `currency` and
`functional_balance_minor`. Assets and expenses take debits minus credits;
liabilities, equity and revenue take credits minus debits. The balance sheet
only reads journals dated on or before `as_of`; the income statement only reads
journals dated inside the period.

```json
{"as_of":"2024-01-31","period_start":"2024-01-01","period_end":"2024-01-31",
 "functional_currency":"CNY",
 "balance_sheet":{
   "assets":[{"account_id":"1100","account_name":"Cash","parent_id":"1000",
     "currency":"CNY","functional_balance_minor":1220700}],
   "liabilities":[],"equity":[{"account_id":"3000","account_name":"Equity",
     "parent_id":null,"currency":"CNY","functional_balance_minor":1000000}],
   "assets_total_minor":1220700,"liabilities_total_minor":0,
   "equity_total_minor":1220700,"balance_check_minor":0,"status":"balanced"},
 "income_statement":{
   "revenue":[{"account_id":"4000","account_name":"Revenue","parent_id":null,
     "currency":"CNY","functional_balance_minor":300000}],
   "expenses":[{"account_id":"5000","account_name":"Rent","parent_id":null,
     "currency":"CNY","functional_balance_minor":80000}],
   "revenue_total_minor":300000,"expenses_total_minor":80000,
   "net_income_minor":220000}}
```

`equity_total_minor` is the equity accounts' credit-minus-debit total through
`as_of` plus revenue minus expenses through `as_of`, so the accounting equation
holds even before earnings are closed into equity. `balance_check_minor` is
`assets_total_minor - liabilities_total_minor - equity_total_minor`; the balance
sheet `status` is `balanced` when it is zero and `unbalanced` otherwise.
`net_income_minor` uses only revenue minus expenses inside the stated period.
An empty ledger returns empty account arrays, zero totals and `balanced`.

### Store an exchange rate snapshot

```http
POST /rates
Idempotency-Key: rate-1

{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.245"}
```

Returns HTTP 201. The decimal string is reduced to an exact fraction, which is
returned as well. `GET /rates` lists every snapshot ordered by `base`, `quote`
and `date`.

```json
{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.245",
 "numerator":1449,"denominator":200,"created_at":"2024-06-01T12:00:00Z"}
```

### Post a journal

```http
POST /journals
Idempotency-Key: journal-1

{"id":"jv-1","date":"2024-01-15","memo":"consulting invoice",
 "lines":[
   {"account_id":"1200","side":"debit","amount_minor":100000,"reference":"INV-1"},
   {"account_id":"4200","side":"credit","amount_minor":100000,"reference":"INV-1"}
 ]}
```

Returns HTTP 201 with the stored journal. Both lines are in USD, so the
2024-01-01 snapshot applies and each line carries its resolved rate;
`GET /journals/{id}` returns the same document.

```json
{"id":"jv-1","date":"2024-01-15","memo":"consulting invoice",
 "functional_currency":"CNY","debit_functional_minor":724500,
 "credit_functional_minor":724500,"created_at":"2024-06-01T12:00:00Z","lines":[
   {"index":1,"account_id":"1200","side":"debit","amount_minor":100000,
    "currency":"USD","rate":"","rate_numerator":1449,"rate_denominator":200,
    "rate_source":"snapshot:2024-01-01","functional_amount_minor":724500,
    "reference":"INV-1"},
   {"index":2,"account_id":"4200","side":"credit","amount_minor":100000,
    "currency":"USD","rate":"","rate_numerator":1449,"rate_denominator":200,
    "rate_source":"snapshot:2024-01-01","functional_amount_minor":724500,
    "reference":"INV-1"}]}
```

A journal whose functional debits and credits differ returns HTTP 400, for
example `journal is not balanced in CNY: debits 100000, credits 90000`.

### Reverse a journal

```http
POST /journals/jv-1/reversals
Idempotency-Key: reversal-1

{"id":"jv-1-r","date":"2024-02-10","memo":"customer cancelled"}
```

Creates exactly one reversal journal for the journal named in the path and
returns HTTP 201 with it; `GET /journals/jv-1-r` returns the same document and
the original journal is never modified. `id` follows the journal id rules,
`date` must be a valid calendar date not before the original journal's date,
and `memo` is a non-empty reason. Every line keeps the original `account_id`,
`amount_minor`, `currency`, `reference`, resolved rate fraction, `rate_source`
and `functional_amount_minor` with only `debit` and `credit` exchanged, so the
reversal's functional debit total equals the original's credit total and the
pair cancels exactly. Balances, the trial balance and the financial statements
before the reversal date are unchanged; from that date on they include the
reversal lines like any other posting.

```json
{"id":"jv-1-r","date":"2024-02-10","memo":"customer cancelled",
 "functional_currency":"CNY","debit_functional_minor":724500,
 "credit_functional_minor":724500,"reversal_of":"jv-1",
 "created_at":"2024-06-01T12:00:00Z","lines":[
   {"index":1,"account_id":"1200","side":"credit","amount_minor":100000,
    "currency":"USD","rate":"","rate_numerator":1449,"rate_denominator":200,
    "rate_source":"snapshot:2024-01-01","functional_amount_minor":724500,
    "reference":"INV-1"},
   {"index":2,"account_id":"4200","side":"debit","amount_minor":100000,
    "currency":"USD","rate":"","rate_numerator":1449,"rate_denominator":200,
    "rate_source":"snapshot:2024-01-01","functional_amount_minor":724500,
    "reference":"INV-1"}]}
```

An unknown original id returns HTTP 404. A `date` before the original date is
HTTP 400. HTTP 409 results — writing nothing — are a reused `id`, an original
that is itself a reversal, an original that was already reversed, or a
reversal date inside a closed month. The reversal journal and the record that
the original has been reversed are persisted atomically, so a restart cannot
allow a second reversal.

### Close a period

```http
POST /period-closes
Idempotency-Key: close-1

{"id":"pc-2024-01","period":{"start":"2024-01-01","end":"2024-01-31"}}
```

Returns HTTP 201 with the stored close; `GET /period-closes/{id}` returns it.
The period must be exactly one calendar month and the month must not be closed
already.

```json
{"id":"pc-2024-01","period":{"start":"2024-01-01","end":"2024-01-31"},
 "status":"closed","closed_at":"2024-06-01T12:00:00Z",
 "adjustment_count":0,"adjustment_journal_ids":[]}
```

After a month is closed, `POST /journals` dated inside it returns HTTP 409
with `closed period rejects journal`.

### Post an adjustment into a closed period

```http
POST /period-closes/pc-2024-01/adjustments
Idempotency-Key: adj-1

{"id":"jv-adj-1","date":"2024-01-31","memo":"accrue January interest",
 "lines":[
   {"account_id":"1000","side":"debit","amount_minor":5000},
   {"account_id":"4000","side":"credit","amount_minor":5000}
 ]}
```

Returns HTTP 201 with the stored journal, readable through
`GET /journals/{id}` like any other voucher, and increments the close's
`adjustment_count` and `adjustment_journal_ids`. The adjustment follows every
ordinary journal rule; `memo` must be non-empty and `date` must fall inside
the closed period.

### Import a bank statement

```http
POST /statements
Idempotency-Key: statement-1

{"id":"st-1","account_id":"1200","currency":"USD",
 "period":{"start":"2024-01-01","end":"2024-01-31"},
 "opening_balance_minor":0,"closing_balance_minor":87000,
 "lines":[
   {"id":"s1","date":"2024-01-15","amount_minor":100000,"reference":"INV-1"},
   {"id":"s2","date":"2024-01-28","amount_minor":-13000,"reference":"FEE-1"}
 ]}
```

Returns HTTP 201 with the stored statement; `GET /statements/{id}` returns it.
A statement whose lines do not add up to the closing balance returns HTTP 400.

### Reconcile a statement

```http
POST /reconciliations
Idempotency-Key: recon-1

{"id":"rec-1","statement_id":"st-1"}
```

Returns HTTP 201 with the frozen result:

```json
{"id":"rec-1","statement_id":"st-1","account_id":"1200","currency":"USD",
 "functional_currency":"CNY","period":{"start":"2024-01-01","end":"2024-01-31"},
 "status":"differences_found","matched_count":1,"statement_line_count":2,
 "ledger_line_count":1,"statement_net_minor":87000,"ledger_net_minor":100000,
 "difference_minor":-13000,
 "summary":{"timing":0,"amount_mismatch":0,"missing_in_ledger":1,
            "missing_in_statement":0},
 "differences":[
   {"type":"missing_in_ledger",
    "detail":"the statement line has no journal line in the period",
    "statement_line_id":"s2","statement_date":"2024-01-28",
    "statement_amount_minor":-13000,"difference_minor":-13000}],
 "created_at":"2024-06-01T12:00:00Z"}
```

`GET /reconciliations/{id}` returns the stored reconciliation unchanged, even
after more journals are posted; create a new reconciliation to refresh it.

## Errors

Errors use this shape:

```json
{"error":{"code":"validation_error","message":"journal is not balanced in CNY: debits 100000, credits 90000"}}
```

| Code | Status | Raised when |
| --- | --- | --- |
| `validation_error` | 400 | the body or a parameter violates the contract, or the idempotency key is missing |
| `not_found` | 404 | unknown route or unknown id |
| `conflict` | 409 | duplicate id, duplicate rate snapshot, a journal or reversal dated in a closed period, a journal that is already reversed or is itself a reversal, or an idempotency key reused for another operation |
| `internal_error` | 500 | an internal invariant, such as the reconciliation completeness check, failed |

## Tests

```bash
go test ./... -v
```
