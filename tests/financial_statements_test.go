package tests

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func financialStatementsPath(asOf, start, end string) string {
	return fmt.Sprintf(
		"/reports/financial-statements?as_of=%s&period_start=%s&period_end=%s",
		asOf, start, end,
	)
}

func statementObject(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	value, ok := body[name].(map[string]any)
	if !ok {
		t.Fatalf("field %s is %v, want an object", name, body[name])
	}
	return value
}

func rowByID(rows []map[string]any) map[string]map[string]any {
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[row["account_id"].(string)] = row
	}
	return byID
}

func TestFinancialStatementsEmptyLedger(t *testing.T) {
	client := newClient(t)

	report := client.ok(financialStatementsPath("2024-12-31", "2024-12-01", "2024-12-31"))
	if text(t, report, "functional_currency") != "CNY" {
		t.Fatalf("functional currency is %v", report["functional_currency"])
	}
	balanceSheet := statementObject(t, report, "balance_sheet")
	incomeStatement := statementObject(t, report, "income_statement")
	if text(t, balanceSheet, "as_of") != "2024-12-31" {
		t.Fatalf("balance sheet as_of is %v", balanceSheet["as_of"])
	}
	if text(t, incomeStatement, "period_start") != "2024-12-01" ||
		text(t, incomeStatement, "period_end") != "2024-12-31" {
		t.Fatalf("income statement period is %v", incomeStatement)
	}
	for _, bucket := range []string{"assets", "liabilities", "equity"} {
		if len(objects(t, balanceSheet, bucket)) != 0 {
			t.Fatalf("empty ledger %s must be an empty array: %v", bucket, balanceSheet[bucket])
		}
	}
	for _, bucket := range []string{"revenue", "expenses"} {
		if len(objects(t, incomeStatement, bucket)) != 0 {
			t.Fatalf("empty ledger %s must be an empty array: %v", bucket, incomeStatement[bucket])
		}
	}
	for _, name := range []string{
		"assets_total_minor", "liabilities_total_minor", "equity_total_minor",
		"balance_check_minor",
	} {
		if integer(t, balanceSheet, name) != 0 {
			t.Fatalf("%s is %v, want zero", name, balanceSheet[name])
		}
	}
	if text(t, balanceSheet, "status") != "balanced" {
		t.Fatalf("empty balance sheet status is %v", balanceSheet["status"])
	}
	if integer(t, incomeStatement, "revenue_total_minor") != 0 ||
		integer(t, incomeStatement, "expenses_total_minor") != 0 ||
		integer(t, incomeStatement, "net_income_minor") != 0 {
		t.Fatalf("empty income statement totals are %v", incomeStatement)
	}
}

// seedFinancialStatementsLedger builds a multi-currency ledger spanning
// December 2023 (opening equity), January 2024 (USD sale, CNY expense) and
// February 2024 (CNY sale).
func seedFinancialStatementsLedger(t *testing.T, client *client) {
	t.Helper()
	client.created("/accounts", accountBody("1000", "Assets", "asset", "CNY"))
	client.created("/accounts", `{"id":"1100","name":"CNY cash","type":"asset","parent_id":"1000","currency":"CNY"}`)
	client.created("/accounts", `{"id":"1200","name":"USD cash","type":"asset","parent_id":"1000","currency":"USD"}`)
	client.created("/accounts", accountBody("2000", "Loan", "liability", "CNY"))
	client.created("/accounts", accountBody("3000", "Equity", "equity", "CNY"))
	client.created("/accounts", accountBody("4000", "CNY revenue", "revenue", "CNY"))
	client.created("/accounts", accountBody("4200", "USD revenue", "revenue", "USD"))
	client.created("/accounts", accountBody("5000", "Rent", "expense", "CNY"))
	client.created("/rates", `{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.0"}`)

	client.created("/journals", `{
		"id":"jv-opening","date":"2023-12-31","memo":"opening equity",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":100000},
			{"account_id":"3000","side":"credit","amount_minor":100000}
		]}`)
	client.created("/journals", `{
		"id":"jv-usd","date":"2024-01-20",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":10000},
			{"account_id":"4200","side":"credit","amount_minor":10000}
		]}`)
	client.created("/journals", `{
		"id":"jv-rent","date":"2024-01-25",
		"lines":[
			{"account_id":"5000","side":"debit","amount_minor":20000},
			{"account_id":"1100","side":"credit","amount_minor":20000}
		]}`)
	client.created("/journals", `{
		"id":"jv-feb","date":"2024-02-10",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":50000},
			{"account_id":"4000","side":"credit","amount_minor":50000}
		]}`)
}

func TestFinancialStatementsFebruary(t *testing.T) {
	client := newClient(t)
	seedFinancialStatementsLedger(t, client)

	report := client.ok(financialStatementsPath("2024-02-29", "2024-02-01", "2024-02-29"))
	if text(t, report, "functional_currency") != "CNY" {
		t.Fatalf("functional currency is %v", report["functional_currency"])
	}
	balanceSheet := statementObject(t, report, "balance_sheet")
	incomeStatement := statementObject(t, report, "income_statement")

	assets := objects(t, balanceSheet, "assets")
	liabilities := objects(t, balanceSheet, "liabilities")
	equities := objects(t, balanceSheet, "equity")
	if len(assets) != 3 || len(liabilities) != 1 || len(equities) != 1 {
		t.Fatalf("balance sheet rows are assets=%v liabilities=%v equity=%v", assets, liabilities, equities)
	}
	wantAssetIDs := []string{"1000", "1100", "1200"}
	for index, want := range wantAssetIDs {
		if text(t, assets[index], "account_id") != want {
			t.Fatalf("assets are not id-sorted: %v", assets)
		}
	}
	if text(t, liabilities[0], "account_id") != "2000" || text(t, equities[0], "account_id") != "3000" {
		t.Fatalf("liability/equity rows are %v %v", liabilities, equities)
	}

	assetRows := rowByID(assets)
	// The parent account keeps its own zero row: children never roll up.
	if integer(t, assetRows["1000"], "functional_balance_minor") != 0 {
		t.Fatalf("parent account rolled up its children: %v", assetRows["1000"])
	}
	// 100000 opening debit, 20000 January rent credit, 50000 February debit.
	if integer(t, assetRows["1100"], "functional_balance_minor") != 130000 {
		t.Fatalf("CNY cash balance is %v", assetRows["1100"])
	}
	// The stored functional amount (10000 USD minor at 7.0 = 70000) is reused.
	if integer(t, assetRows["1200"], "functional_balance_minor") != 70000 {
		t.Fatalf("USD cash functional balance is %v", assetRows["1200"])
	}
	if text(t, assetRows["1200"], "currency") != "USD" || text(t, assetRows["1200"], "account_name") != "USD cash" {
		t.Fatalf("USD row identity fields are %v", assetRows["1200"])
	}
	if assetRows["1100"]["parent_id"] != "1000" || assetRows["1000"]["parent_id"] != nil {
		t.Fatalf("parent_id linkage is %v and %v", assetRows["1100"]["parent_id"], assetRows["1000"]["parent_id"])
	}
	if integer(t, rowByID(liabilities)["2000"], "functional_balance_minor") != 0 {
		t.Fatalf("unused liability must be zero: %v", liabilities[0])
	}
	if integer(t, rowByID(equities)["3000"], "functional_balance_minor") != 100000 {
		t.Fatalf("equity row is %v", equities[0])
	}

	// assets 200000 = equity 100000 + revenue 120000 - expenses 20000.
	if integer(t, balanceSheet, "assets_total_minor") != 200000 {
		t.Fatalf("assets total is %v", balanceSheet["assets_total_minor"])
	}
	if integer(t, balanceSheet, "liabilities_total_minor") != 0 {
		t.Fatalf("liabilities total is %v", balanceSheet["liabilities_total_minor"])
	}
	if integer(t, balanceSheet, "equity_total_minor") != 200000 {
		t.Fatalf("equity total is %v", balanceSheet["equity_total_minor"])
	}
	if integer(t, balanceSheet, "balance_check_minor") != 0 ||
		text(t, balanceSheet, "status") != "balanced" {
		t.Fatalf("balance check is %v with status %v",
			balanceSheet["balance_check_minor"], balanceSheet["status"])
	}

	revenue := objects(t, incomeStatement, "revenue")
	expenses := objects(t, incomeStatement, "expenses")
	if len(revenue) != 2 || len(expenses) != 1 {
		t.Fatalf("income statement rows are revenue=%v expenses=%v", revenue, expenses)
	}
	revenueRows := rowByID(revenue)
	if text(t, revenue[0], "account_id") != "4000" || text(t, revenue[1], "account_id") != "4200" {
		t.Fatalf("revenue rows are not id-sorted: %v", revenue)
	}
	// February only: the January USD sale stays out of the period rows.
	if integer(t, revenueRows["4000"], "functional_balance_minor") != 50000 {
		t.Fatalf("February CNY revenue is %v", revenueRows["4000"])
	}
	if integer(t, revenueRows["4200"], "functional_balance_minor") != 0 {
		t.Fatalf("January USD revenue leaked into February: %v", revenueRows["4200"])
	}
	if integer(t, rowByID(expenses)["5000"], "functional_balance_minor") != 0 {
		t.Fatalf("January rent leaked into February: %v", expenses[0])
	}
	if integer(t, incomeStatement, "revenue_total_minor") != 50000 ||
		integer(t, incomeStatement, "expenses_total_minor") != 0 ||
		integer(t, incomeStatement, "net_income_minor") != 50000 {
		t.Fatalf("February income statement totals are %v", incomeStatement)
	}
}

func TestFinancialStatementsJanuary(t *testing.T) {
	client := newClient(t)
	seedFinancialStatementsLedger(t, client)

	// A one-day closed interval on the month end is legal.
	report := client.ok(financialStatementsPath("2024-01-31", "2024-01-01", "2024-01-31"))
	balanceSheet := statementObject(t, report, "balance_sheet")
	incomeStatement := statementObject(t, report, "income_statement")

	assets := rowByID(objects(t, balanceSheet, "assets"))
	// 100000 opening debit minus 20000 rent credit.
	if integer(t, assets["1100"], "functional_balance_minor") != 80000 {
		t.Fatalf("January CNY cash is %v", assets["1100"])
	}
	if integer(t, assets["1200"], "functional_balance_minor") != 70000 {
		t.Fatalf("January USD cash is %v", assets["1200"])
	}
	if integer(t, balanceSheet, "assets_total_minor") != 150000 ||
		integer(t, balanceSheet, "equity_total_minor") != 150000 ||
		integer(t, balanceSheet, "balance_check_minor") != 0 {
		t.Fatalf("January balance sheet totals are %v", balanceSheet)
	}

	revenue := rowByID(objects(t, incomeStatement, "revenue"))
	expenses := rowByID(objects(t, incomeStatement, "expenses"))
	if integer(t, revenue["4200"], "functional_balance_minor") != 70000 ||
		integer(t, revenue["4000"], "functional_balance_minor") != 0 {
		t.Fatalf("January revenue rows are %v", revenue)
	}
	if integer(t, expenses["5000"], "functional_balance_minor") != 20000 {
		t.Fatalf("January expense row is %v", expenses["5000"])
	}
	if integer(t, incomeStatement, "revenue_total_minor") != 70000 ||
		integer(t, incomeStatement, "expenses_total_minor") != 20000 ||
		integer(t, incomeStatement, "net_income_minor") != 50000 {
		t.Fatalf("January income statement totals are %v", incomeStatement)
	}

	// The February journal never enters an as_of answer dated in January.
	if integer(t, assets["1100"], "functional_balance_minor") == 130000 {
		t.Fatalf("February journal leaked into the January balance sheet")
	}
}

func TestFinancialStatementsValidation(t *testing.T) {
	client := newClient(t)
	base := "/reports/financial-statements"

	client.expect(
		request{method: http.MethodGet, path: base + "?period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "as_of query parameter is required",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_end=2024-01-31"},
		400, "validation_error", "period_start query parameter is required",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01"},
		400, "validation_error", "period_end query parameter is required",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-02-30&period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "ISO 8601",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-13-01&period_end=2024-01-31"},
		400, "validation_error", "ISO 8601",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01&period_end=not-a-date"},
		400, "validation_error", "ISO 8601",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-15&period_start=2024-01-20&period_end=2024-01-15"},
		400, "validation_error", "period_start must not be after period_end",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-20&period_start=2024-01-01&period_end=2024-01-21"},
		400, "validation_error", "period_end must not be after as_of",
	)
	// Repeated parameters are rejected in as_of, period_start, period_end order.
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&as_of=2024-02-01&period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "as_of must appear exactly once",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01&period_start=2024-01-02&period_end=2024-01-31"},
		400, "validation_error", "period_start must appear exactly once",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01&period_end=2024-01-31&period_end=2024-02-01"},
		400, "validation_error", "period_end must appear exactly once",
	)
	client.expect(
		request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01&period_end=2024-01-31&unknown=1"},
		400, "validation_error", "unknown query parameter",
	)
	// Equal dates satisfy period_start <= period_end <= as_of.
	report := client.ok(base + "?as_of=2024-01-31&period_start=2024-01-31&period_end=2024-01-31")
	if text(t, statementObject(t, report, "balance_sheet"), "status") != "balanced" {
		t.Fatalf("same-day interval must be accepted: %v", report)
	}
	client.expect(request{method: http.MethodPost, path: base}, 404, "not_found", "")
}

// TestFinancialStatementsUnbalancedFromCorruptStore seeds an imbalanced
// journal, which the validating write API can never produce, to prove the
// balance sheet derives its own status from balance_check_minor.
func TestFinancialStatementsUnbalancedFromCorruptStore(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	document := `{
		"accounts": {
			"1000": {"id":"1000","name":"Cash","type":"asset","parent_id":null,"currency":"CNY","normal_balance":"debit","created_at":"2024-06-01T12:00:00Z"},
			"4000": {"id":"4000","name":"Revenue","type":"revenue","parent_id":null,"currency":"CNY","normal_balance":"credit","created_at":"2024-06-01T12:00:00Z"}
		},
		"journals": {
			"jv-bad": {
				"id":"jv-bad","date":"2024-01-15","functional_currency":"CNY",
				"debit_functional_minor":100,"credit_functional_minor":100,
				"created_at":"2024-06-01T12:00:00Z",
				"lines":[
					{"index":1,"account_id":"1000","side":"debit","amount_minor":100,"currency":"CNY","rate":"","rate_numerator":1,"rate_denominator":1,"rate_source":"identity","functional_amount_minor":100},
					{"index":2,"account_id":"4000","side":"credit","amount_minor":60,"currency":"CNY","rate":"","rate_numerator":1,"rate_denominator":1,"rate_source":"identity","functional_amount_minor":60}
				]
			}
		},
		"rates": [], "statements": {}, "reconciliations": {}, "period_closes": {}, "idempotency": {}
	}`
	if err := os.WriteFile(database, []byte(document), 0o600); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	client := newClientOn(t, database)

	report := client.ok(financialStatementsPath("2024-12-31", "2024-01-01", "2024-12-31"))
	balanceSheet := statementObject(t, report, "balance_sheet")
	if integer(t, balanceSheet, "assets_total_minor") != 100 {
		t.Fatalf("corrupt assets total is %v", balanceSheet["assets_total_minor"])
	}
	if integer(t, balanceSheet, "equity_total_minor") != 60 {
		t.Fatalf("corrupt equity total is %v", balanceSheet["equity_total_minor"])
	}
	if integer(t, balanceSheet, "balance_check_minor") != 40 {
		t.Fatalf("corrupt balance check is %v, want 40", balanceSheet["balance_check_minor"])
	}
	if text(t, balanceSheet, "status") != "unbalanced" {
		t.Fatalf("corrupt balance sheet status is %v", balanceSheet["status"])
	}
	incomeStatement := statementObject(t, report, "income_statement")
	if integer(t, incomeStatement, "net_income_minor") != 60 {
		t.Fatalf("corrupt income statement is %v", incomeStatement)
	}
}
