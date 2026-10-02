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
- every reconciliation difference is `timing`, `amount_mismatch`, `missing_in_ledger` or `missing_in_statement`;
- a calendar month can be closed: ordinary journals are then rejected for the
  month and only adjustment journals posted through the close record enter it;
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
immutable and posting never rewrites history.

**Balances.** `GET /accounts/{id}/balance?as_of=` sums the lines of that account
in journals with `date <= as_of` and returns `debit_minor`, `credit_minor`,
`net_minor` (debits minus credits), `balance_minor` (`net_minor` oriented by
`normal_balance`, so a credit-normal account grows when credited), `posting_count`,
the first and last posting date, and the same totals in the functional currency.
Journals dated after `as_of` are not read at all.

**Statements.** `POST /statements` imports one statement for one account.
`period` is `{start, end}` and every line date must fall inside it.
`opening_balance_minor + Σ line amount_minor` must equal `closing_balance_minor`.
Line amounts are signed: positive is money into the account (a debit to it),
negative is money out. Line ids must be unique inside the statement.

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

**Period closes.** `POST /period-closes` closes exactly one complete calendar
month, given as `period.start` on day 1 and `period.end` on the last day of the
same month; the record is immutable. Once a month is closed, `POST /journals`
rejects any journal dated inside it with `conflict`
(`closed period rejects journal`), while journals dated outside the month post
normally. Adjustments still enter the closed month through
`POST /period-closes/{id}/adjustments`: the body is a normal journal body
(`id`, `date`, non-empty `memo`, `lines`) with the same rate resolution,
rounding, two-line minimum and functional-currency balancing rules, and the
date must fall inside the closed month. Every accepted adjustment is a normal
immutable journal readable via `GET /journals/{id}` and counted on the close
record in `adjustment_count` and `adjustment_journal_ids`.

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

### Close a period

```http
POST /period-closes
Idempotency-Key: close-2024-02

{"id":"pc-2024-02","period":{"start":"2024-02-01","end":"2024-02-29"}}
```

Returns HTTP 201 with the close record. `status` is `closed` and stays that
way; `adjustment_count` starts at 0 and `adjustment_journal_ids` starts empty.
`GET /period-closes/{id}` returns the same record, updated as adjustments
arrive.

```json
{"id":"pc-2024-02","period":{"start":"2024-02-01","end":"2024-02-29"},
 "status":"closed","closed_at":"2024-06-01T12:00:00Z",
 "adjustment_count":0,"adjustment_journal_ids":[]}
```

The same `id` cannot be used twice (`period close exists`), and the same
calendar month cannot be closed twice (`calendar month is closed`). A period
that is not exactly one calendar month is rejected with
`period is not one calendar month`.

### Post an adjustment into a closed period

```http
POST /period-closes/pc-2024-02/adjustments
Idempotency-Key: adj-1

{"id":"jv-adj-1","date":"2024-02-15","memo":"accrue missing bank fee",
 "lines":[
   {"account_id":"5000","side":"debit","amount_minor":1200},
   {"account_id":"1100","side":"credit","amount_minor":1200}
 ]}
```

Returns HTTP 201 with the stored journal, exactly like `POST /journals`. The
`memo` is required (`adjustment memo required`), the date must fall inside the
closed month (`adjustment date outside period`), an existing journal id is
rejected with `journal exists`, and all other journal validation keeps its
original codes and messages. After success the close record carries the new
journal id and an incremented count.

## Errors

Errors use this shape:

```json
{"error":{"code":"validation_error","message":"journal is not balanced in CNY: debits 100000, credits 90000"}}
```

| Code | Status | Raised when |
| --- | --- | --- |
| `validation_error` | 400 | the body or a parameter violates the contract, including `invalid period close id`, `invalid period date`, `period is not one calendar month`, `adjustment memo required` or `adjustment date outside period`, or the idempotency key is missing |
| `not_found` | 404 | unknown route, unknown id, or `period close not found` |
| `conflict` | 409 | duplicate id, duplicate rate snapshot, an idempotency key reused for another operation, a second close of the same calendar month (`calendar month is closed`), `journal exists`, or `closed period rejects journal` for an ordinary journal dated in a closed month |
| `internal_error` | 500 | an internal invariant, such as the reconciliation completeness check, failed |

## Tests

```bash
go test ./... -v
```
