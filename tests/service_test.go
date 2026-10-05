package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"reconcore/internal/reconcore"
)

// fixedClock makes every created_at value reproducible.
var fixedClock = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

type request struct {
	method      string
	path        string
	key         string
	body        string
	contentType string
	omitType    bool
}

type client struct {
	t      *testing.T
	server *httptest.Server
	keys   int
}

func newClient(t *testing.T) *client {
	t.Helper()
	return newClientOn(t, filepath.Join(t.TempDir(), "ledger.db"))
}

func newClientOn(t *testing.T, database string) *client {
	t.Helper()
	store, err := reconcore.OpenStore(database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	service, err := reconcore.NewService(store, "CNY", func() time.Time { return fixedClock })
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	server := httptest.NewServer(reconcore.NewServer(service))
	t.Cleanup(server.Close)
	return &client{t: t, server: server}
}

func (c *client) send(r request) (int, string, map[string]any) {
	c.t.Helper()
	var body io.Reader
	if r.body != "" {
		body = bytes.NewBufferString(r.body)
	}
	httpRequest, err := http.NewRequest(r.method, c.server.URL+r.path, body)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	if !r.omitType {
		contentType := r.contentType
		if contentType == "" {
			contentType = "application/json"
		}
		httpRequest.Header.Set("Content-Type", contentType)
	}
	if r.key != "" {
		httpRequest.Header.Set("Idempotency-Key", r.key)
	}
	response, err := c.server.Client().Do(httpRequest)
	if err != nil {
		c.t.Fatalf("%s %s: %v", r.method, r.path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		c.t.Fatalf("read %s %s: %v", r.method, r.path, err)
	}
	decoded := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			c.t.Fatalf("%s %s returned non-object JSON %s", r.method, r.path, raw)
		}
	}
	return response.StatusCode, string(raw), decoded
}

func (c *client) post(path, body string) (int, string, map[string]any) {
	c.keys++
	return c.send(request{method: http.MethodPost, path: path, key: fmt.Sprintf("key-%03d", c.keys), body: body})
}

func (c *client) get(path string) (int, string, map[string]any) {
	return c.send(request{method: http.MethodGet, path: path})
}

func (c *client) created(path, body string) map[string]any {
	c.t.Helper()
	status, raw, decoded := c.post(path, body)
	if status != http.StatusCreated {
		c.t.Fatalf("POST %s returned %d: %s", path, status, raw)
	}
	return decoded
}

func (c *client) ok(path string) map[string]any {
	c.t.Helper()
	status, raw, decoded := c.get(path)
	if status != http.StatusOK {
		c.t.Fatalf("GET %s returned %d: %s", path, status, raw)
	}
	return decoded
}

func (c *client) expect(r request, wantStatus int, wantCode, wantMessage string) {
	c.t.Helper()
	status, raw, decoded := c.send(r)
	if status != wantStatus {
		c.t.Fatalf("%s %s returned %d (%s), want %d", r.method, r.path, status, raw, wantStatus)
	}
	failure, ok := decoded["error"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s %s did not return an error body: %s", r.method, r.path, raw)
	}
	if failure["code"] != wantCode {
		c.t.Fatalf("%s %s error code is %v, want %s", r.method, r.path, failure["code"], wantCode)
	}
	message, _ := failure["message"].(string)
	if wantMessage != "" && !strings.Contains(message, wantMessage) {
		c.t.Fatalf("%s %s message %q does not mention %q", r.method, r.path, message, wantMessage)
	}
}

func accountBody(id, name, kind, currency string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"type":%q,"parent_id":null,"currency":%q}`, id, name, kind, currency)
}

func integer(t *testing.T, container map[string]any, name string) int64 {
	t.Helper()
	raw, found := container[name]
	if !found {
		t.Fatalf("field %s is missing in %v", name, container)
	}
	value, ok := raw.(float64)
	if !ok || value != math.Trunc(value) {
		t.Fatalf("field %s is %v, want an integer", name, raw)
	}
	return int64(value)
}

func text(t *testing.T, container map[string]any, name string) string {
	t.Helper()
	raw, found := container[name]
	if !found {
		t.Fatalf("field %s is missing in %v", name, container)
	}
	value, ok := raw.(string)
	if !ok {
		t.Fatalf("field %s is %v, want a string", name, raw)
	}
	return value
}

func objects(t *testing.T, container map[string]any, name string) []map[string]any {
	t.Helper()
	raw, found := container[name]
	if !found {
		t.Fatalf("field %s is missing in %v", name, container)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("field %s is %v, want an array", name, raw)
	}
	out := make([]map[string]any, 0, len(items))
	for index, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d] is %v, want an object", name, index, item)
		}
		out = append(out, entry)
	}
	return out
}

func TestHealthAndRoutingErrors(t *testing.T) {
	client := newClient(t)

	health := client.ok("/health")
	if health["status"] != "ok" {
		t.Fatalf("health returned %v", health)
	}
	client.expect(request{method: http.MethodGet, path: "/unknown"}, 404, "not_found", "route was not found")
	client.expect(request{method: http.MethodPost, path: "/health"}, 404, "not_found", "")
	client.expect(
		request{method: http.MethodPost, path: "/accounts", body: accountBody("1000", "Cash", "asset", "CNY")},
		400, "validation_error", "Idempotency-Key",
	)
	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "k-plain", contentType: "text/plain",
			body: accountBody("1000", "Cash", "asset", "CNY")},
		400, "validation_error", "Content-Type",
	)
	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "k-empty", body: ""},
		400, "validation_error", "must be a JSON object",
	)
	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "k-notype", omitType: true,
			body: accountBody("1000", "Cash", "asset", "CNY")},
		400, "validation_error", "Content-Type",
	)
}

func TestAccountTreeAndValidation(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Assets", "asset", "CNY"))
	client.created("/accounts", `{"id":"1001","name":"Cash","type":"asset","parent_id":"1000","currency":"CNY"}`)

	tree := client.ok("/accounts")
	roots := objects(t, tree, "accounts")
	if len(roots) != 1 || text(t, roots[0], "id") != "1000" {
		t.Fatalf("expected one root account, got %v", roots)
	}
	children := objects(t, roots[0], "children")
	if len(children) != 1 || text(t, children[0], "id") != "1001" {
		t.Fatalf("expected account 1001 under 1000, got %v", children)
	}
	if text(t, children[0], "normal_balance") != "debit" {
		t.Fatalf("asset normal balance is %v", children[0]["normal_balance"])
	}

	subtree := client.ok("/accounts/1000")
	if text(t, subtree, "name") != "Assets" || len(objects(t, subtree, "children")) != 1 {
		t.Fatalf("subtree lookup returned %v", subtree)
	}

	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "dup",
			body: accountBody("1000", "Assets again", "asset", "CNY")},
		409, "conflict", "already exists",
	)
	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "bad-parent",
			body: `{"id":"1002","name":"Ghost","type":"asset","parent_id":"9999","currency":"CNY"}`},
		400, "validation_error", "does not exist",
	)
	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "bad-type",
			body: accountBody("1002", "Ghost", "goodwill", "CNY")},
		400, "validation_error", "type must be one of",
	)
	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "bad-currency",
			body: accountBody("1002", "Ghost", "asset", "cny")},
		400, "validation_error", "ISO 4217",
	)
	client.expect(request{method: http.MethodGet, path: "/accounts/9999"}, 404, "not_found", "")
}

func TestJournalMustBalance(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))

	journal := client.created("/journals", `{
		"id":"jv-1","date":"2024-01-15","memo":"first sale",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":100000,"reference":"INV-1"},
			{"account_id":"4000","side":"credit","amount_minor":100000,"reference":"INV-1"}
		]}`)
	if integer(t, journal, "debit_functional_minor") != 100000 {
		t.Fatalf("journal is not balanced as expected: %v", journal)
	}
	lines := objects(t, journal, "lines")
	if len(lines) != 2 || integer(t, lines[0], "index") != 1 || integer(t, lines[1], "index") != 2 {
		t.Fatalf("journal lines are not indexed from one: %v", lines)
	}
	if text(t, lines[0], "rate_source") != "identity" {
		t.Fatalf("functional currency line rate source is %v", lines[0]["rate_source"])
	}
	if got := text(t, client.ok("/journals/jv-1"), "id"); got != "jv-1" {
		t.Fatalf("stored journal id is %s", got)
	}

	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "unbalanced", body: `{
			"id":"jv-2","date":"2024-01-16",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":100000},
				{"account_id":"4000","side":"credit","amount_minor":90000}
			]}`},
		400, "validation_error", "not balanced in CNY",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "single", body: `{
			"id":"jv-3","date":"2024-01-16",
			"lines":[{"account_id":"1000","side":"debit","amount_minor":100000}]}`},
		400, "validation_error", "at least two postings",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "zero", body: `{
			"id":"jv-4","date":"2024-01-16",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":0},
				{"account_id":"4000","side":"credit","amount_minor":0}
			]}`},
		400, "validation_error", "must not be zero",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "unknown-account", body: `{
			"id":"jv-5","date":"2024-01-16",
			"lines":[
				{"account_id":"9999","side":"debit","amount_minor":100},
				{"account_id":"4000","side":"credit","amount_minor":100}
			]}`},
		400, "validation_error", "does not exist",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "currency", body: `{
			"id":"jv-6","date":"2024-01-16",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":100,"currency":"USD"},
				{"account_id":"4000","side":"credit","amount_minor":100}
			]}`},
		400, "validation_error", "must match the currency",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "dup", body: `{
			"id":"jv-1","date":"2024-01-15",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"4000","side":"credit","amount_minor":1}
			]}`},
		409, "conflict", "already exists",
	)
}

func TestMultiCurrencyRatesAndRounding(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1200", "USD cash", "asset", "USD"))
	client.created("/accounts", accountBody("4200", "USD revenue", "revenue", "USD"))
	client.created("/accounts", accountBody("1000", "CNY cash", "asset", "CNY"))
	client.created("/accounts", accountBody("3000", "CNY equity", "equity", "CNY"))

	rate := client.created("/rates", `{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.245"}`)
	if integer(t, rate, "numerator") != 1449 || integer(t, rate, "denominator") != 200 {
		t.Fatalf("7.245 was not reduced to 1449/200: %v", rate)
	}

	snapshot := client.created("/journals", `{
		"id":"jv-usd","date":"2024-01-15",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":100000},
			{"account_id":"4200","side":"credit","amount_minor":100000}
		]}`)
	lines := objects(t, snapshot, "lines")
	if text(t, lines[0], "rate_source") != "snapshot:2024-01-01" {
		t.Fatalf("rate source is %v", lines[0]["rate_source"])
	}
	if integer(t, lines[0], "functional_amount_minor") != 724500 {
		t.Fatalf("1000.00 USD did not convert to 724500 CNY: %v", lines[0])
	}
	if integer(t, snapshot, "debit_functional_minor") != 724500 {
		t.Fatalf("journal functional debit is %v", snapshot["debit_functional_minor"])
	}

	// 0.5 rounds half away from zero, so one minor unit becomes one minor unit.
	half := client.created("/journals", `{
		"id":"jv-half","date":"2024-01-21",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":1,"rate":"0.5"},
			{"account_id":"3000","side":"credit","amount_minor":1}
		]}`)
	if integer(t, objects(t, half, "lines")[0], "functional_amount_minor") != 1 {
		t.Fatalf("half minor unit did not round away from zero: %v", half)
	}

	balance := client.ok("/accounts/1200/balance?as_of=2024-01-31")
	if integer(t, balance, "debit_minor") != 100001 || integer(t, balance, "functional_debit_minor") != 724501 {
		t.Fatalf("USD balance is %v", balance)
	}
	if integer(t, balance, "balance_minor") != 100001 {
		t.Fatalf("debit-normal balance is %v", balance["balance_minor"])
	}

	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "no-rate", body: `{
			"id":"jv-norate","date":"2023-12-01",
			"lines":[
				{"account_id":"1200","side":"debit","amount_minor":100},
				{"account_id":"4200","side":"credit","amount_minor":100}
			]}`},
		400, "validation_error", "no USD/CNY exchange rate snapshot",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "bad-rate", body: `{
			"id":"jv-badrate","date":"2024-01-15",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":100,"rate":"2"},
				{"account_id":"3000","side":"credit","amount_minor":100}
			]}`},
		400, "validation_error", "functional currency",
	)
	client.expect(
		request{method: http.MethodPost, path: "/rates", key: "dup-rate",
			body: `{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.3"}`},
		409, "conflict", "already exists",
	)
	client.expect(
		request{method: http.MethodPost, path: "/rates", key: "same-rate",
			body: `{"base":"USD","quote":"USD","date":"2024-01-01","rate":"1"}`},
		400, "validation_error", "must be different",
	)
	client.expect(
		request{method: http.MethodPost, path: "/rates", key: "exponent",
			body: `{"base":"USD","quote":"CNY","date":"2024-02-01","rate":"7.2e1"}`},
		400, "validation_error", "positive decimal number",
	)
	if rates := objects(t, client.ok("/rates"), "rates"); len(rates) != 1 {
		t.Fatalf("expected exactly one stored rate, got %v", rates)
	}
}

func TestInverseRateSnapshot(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1200", "USD cash", "asset", "USD"))
	client.created("/accounts", accountBody("3000", "CNY equity", "equity", "CNY"))
	client.created("/rates", `{"base":"CNY","quote":"USD","date":"2024-01-01","rate":"0.1379"}`)

	journal := client.created("/journals", `{
		"id":"jv-inverse","date":"2024-03-01",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":100000},
			{"account_id":"3000","side":"credit","amount_minor":725163}
		]}`)
	lines := objects(t, journal, "lines")
	if text(t, lines[0], "rate_source") != "snapshot-inverse:2024-01-01" {
		t.Fatalf("inverse rate source is %v", lines[0]["rate_source"])
	}
	if integer(t, lines[0], "functional_amount_minor") != 725163 {
		t.Fatalf("inverse conversion produced %v", lines[0]["functional_amount_minor"])
	}
}

func TestBalanceAsOfIgnoresLaterJournals(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1100", "Bank", "asset", "CNY"))
	client.created("/accounts", accountBody("2100", "Loan", "liability", "CNY"))
	client.created("/journals", `{
		"id":"jv-jan","date":"2024-01-10",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":100000},
			{"account_id":"2100","side":"credit","amount_minor":100000}
		]}`)
	client.created("/journals", `{
		"id":"jv-feb","date":"2024-02-20",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":250000},
			{"account_id":"2100","side":"credit","amount_minor":250000}
		]}`)

	early := client.ok("/accounts/1100/balance?as_of=2024-01-31")
	if integer(t, early, "net_minor") != 100000 || integer(t, early, "posting_count") != 1 {
		t.Fatalf("January balance is %v", early)
	}
	if text(t, early, "first_posting_date") != "2024-01-10" || text(t, early, "last_posting_date") != "2024-01-10" {
		t.Fatalf("January balance dates are %v", early)
	}

	late := client.ok("/accounts/1100/balance?as_of=2024-12-31")
	if integer(t, late, "debit_minor") != 350000 || integer(t, late, "posting_count") != 2 {
		t.Fatalf("December balance is %v", late)
	}
	if integer(t, late, "balance_minor") != 350000 {
		t.Fatalf("debit-normal balance is %v", late["balance_minor"])
	}

	liability := client.ok("/accounts/2100/balance?as_of=2024-12-31")
	if text(t, liability, "normal_balance") != "credit" {
		t.Fatalf("liability normal balance is %v", liability["normal_balance"])
	}
	if integer(t, liability, "net_minor") != -350000 || integer(t, liability, "balance_minor") != 350000 {
		t.Fatalf("liability balance is %v", liability)
	}
}

func TestStatementValidation(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1100", "Bank", "asset", "CNY"))

	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-imbalance", body: `{
			"id":"st-bad","account_id":"1100","currency":"CNY","period":{"start":"2024-03-01","end":"2024-03-31"},
			"opening_balance_minor":0,"closing_balance_minor":500,
			"lines":[{"id":"l1","date":"2024-03-05","amount_minor":100}]}`},
		400, "validation_error", "do not reconcile with the closing balance",
	)
	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-duplicate", body: `{
			"id":"st-bad","account_id":"1100","currency":"CNY","period":{"start":"2024-03-01","end":"2024-03-31"},
			"opening_balance_minor":0,"closing_balance_minor":200,
			"lines":[{"id":"l1","date":"2024-03-05","amount_minor":100},{"id":"l1","date":"2024-03-06","amount_minor":100}]}`},
		400, "validation_error", "duplicated",
	)
	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-outside", body: `{
			"id":"st-bad","account_id":"1100","currency":"CNY","period":{"start":"2024-03-01","end":"2024-03-31"},
			"opening_balance_minor":0,"closing_balance_minor":100,
			"lines":[{"id":"l1","date":"2024-04-05","amount_minor":100}]}`},
		400, "validation_error", "inside the statement period",
	)
	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-zero", body: `{
			"id":"st-bad","account_id":"1100","currency":"CNY","period":{"start":"2024-03-01","end":"2024-03-31"},
			"opening_balance_minor":0,"closing_balance_minor":0,
			"lines":[{"id":"l1","date":"2024-03-05","amount_minor":0}]}`},
		400, "validation_error", "must not be zero",
	)
	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-currency", body: `{
			"id":"st-bad","account_id":"1100","currency":"USD","period":{"start":"2024-03-01","end":"2024-03-31"},
			"opening_balance_minor":0,"closing_balance_minor":100,
			"lines":[{"id":"l1","date":"2024-03-05","amount_minor":100}]}`},
		400, "validation_error", "must match the currency",
	)
	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-account", body: `{
			"id":"st-bad","account_id":"9999","currency":"CNY","period":{"start":"2024-03-01","end":"2024-03-31"},
			"opening_balance_minor":0,"closing_balance_minor":0,"lines":[]}`},
		400, "validation_error", "does not exist",
	)
	client.expect(
		request{method: http.MethodPost, path: "/statements", key: "s-period", body: `{
			"id":"st-bad","account_id":"1100","currency":"CNY","period":{"start":"2024-03-31","end":"2024-03-01"},
			"opening_balance_minor":0,"closing_balance_minor":0,"lines":[]}`},
		400, "validation_error", "must not be after",
	)
	client.expect(request{method: http.MethodGet, path: "/statements/st-bad"}, 404, "not_found", "")
}

func TestReconciliationClassification(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1100", "Bank", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/accounts", accountBody("5000", "Expense", "expense", "CNY"))

	post := func(id, date string, bankSide string, amount int64, reference string) {
		client.created("/journals", fmt.Sprintf(`{
			"id":%q,"date":%q,
			"lines":[
				{"account_id":"1100","side":%q,"amount_minor":%d,"reference":%q},
				{"account_id":"4000","side":%q,"amount_minor":%d,"reference":%q}
			]}`,
			id, date, bankSide, amount, reference,
			map[string]string{"debit": "credit", "credit": "debit"}[bankSide], amount, reference))
	}
	post("jv-1", "2024-03-05", "debit", 100000, "INV-1")
	post("jv-2", "2024-03-08", "credit", 50000, "INV-2")
	post("jv-3", "2024-03-10", "debit", 25000, "INV-3")
	post("jv-4", "2024-03-15", "credit", 12000, "INV-5")
	post("jv-5", "2024-04-02", "debit", 999, "INV-6")

	client.created("/statements", `{
		"id":"st-1","account_id":"1100","currency":"CNY",
		"period":{"start":"2024-03-01","end":"2024-03-31"},
		"opening_balance_minor":0,"closing_balance_minor":87000,
		"lines":[
			{"id":"s1","date":"2024-03-05","amount_minor":100000,"reference":"INV-1"},
			{"id":"s2","date":"2024-03-06","amount_minor":-50000,"reference":"INV-2"},
			{"id":"s3","date":"2024-03-10","amount_minor":30000,"reference":"INV-3"},
			{"id":"s4","date":"2024-03-12","amount_minor":7000,"reference":"INV-4"}
		]}`)

	reconciliation := client.created("/reconciliations", `{"id":"rec-1","statement_id":"st-1"}`)
	if text(t, reconciliation, "status") != "differences_found" {
		t.Fatalf("status is %v", reconciliation["status"])
	}
	if integer(t, reconciliation, "matched_count") != 3 {
		t.Fatalf("matched count is %v", reconciliation["matched_count"])
	}
	if integer(t, reconciliation, "statement_line_count") != 4 || integer(t, reconciliation, "ledger_line_count") != 4 {
		t.Fatalf("line counts are %v", reconciliation)
	}
	// The 2024-04-02 journal sits outside the period and must not be counted.
	if integer(t, reconciliation, "statement_net_minor") != 87000 || integer(t, reconciliation, "ledger_net_minor") != 63000 {
		t.Fatalf("nets are %v", reconciliation)
	}
	if integer(t, reconciliation, "difference_minor") != 24000 {
		t.Fatalf("difference is %v", reconciliation["difference_minor"])
	}
	summary, ok := reconciliation["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary is %v", reconciliation["summary"])
	}
	for name, want := range map[string]int64{
		"timing": 1, "amount_mismatch": 1, "missing_in_ledger": 1, "missing_in_statement": 1,
	} {
		if integer(t, summary, name) != want {
			t.Fatalf("summary.%s is %v, want %d", name, summary[name], want)
		}
	}

	differences := objects(t, reconciliation, "differences")
	wantTypes := []string{"timing", "amount_mismatch", "missing_in_ledger", "missing_in_statement"}
	if len(differences) != len(wantTypes) {
		t.Fatalf("expected four differences, got %v", differences)
	}
	var explained int64
	for index, difference := range differences {
		if text(t, difference, "type") != wantTypes[index] {
			t.Fatalf("difference %d is %v, want %s", index, difference["type"], wantTypes[index])
		}
		explained += integer(t, difference, "difference_minor")
	}
	if explained != integer(t, reconciliation, "difference_minor") {
		t.Fatalf("differences explain %d but the period difference is %v", explained, reconciliation["difference_minor"])
	}
	if text(t, differences[0], "statement_line_id") != "s2" || text(t, differences[0], "ledger_line_id") != "jv-2#1" {
		t.Fatalf("timing pair is %v", differences[0])
	}
	if integer(t, differences[1], "difference_minor") != 5000 {
		t.Fatalf("amount mismatch is %v", differences[1])
	}
	// A ledger line with no statement line contributes its statement-side
	// equivalent, so a missing credit of 12000 shows up as +12000.
	if integer(t, differences[3], "difference_minor") != 12000 {
		t.Fatalf("missing_in_statement is %v", differences[3])
	}
	if integer(t, differences[3], "ledger_amount_minor") != -12000 {
		t.Fatalf("missing_in_statement ledger amount is %v", differences[3])
	}

	// A frozen reconciliation is durable and readable.
	stored := client.ok("/reconciliations/rec-1")
	if integer(t, stored, "difference_minor") != 24000 || len(objects(t, stored, "differences")) != 4 {
		t.Fatalf("stored reconciliation is %v", stored)
	}
	client.expect(request{method: http.MethodGet, path: "/reconciliations/rec-9"}, 404, "not_found", "")

	balance := client.ok("/accounts/1100/balance?as_of=2024-03-31")
	if integer(t, balance, "debit_minor") != 125000 || integer(t, balance, "credit_minor") != 62000 {
		t.Fatalf("ledger balance is %v", balance)
	}
	if integer(t, balance, "net_minor") != integer(t, reconciliation, "ledger_net_minor") {
		t.Fatalf("ledger net %v disagrees with the balance %v", reconciliation["ledger_net_minor"], balance["net_minor"])
	}

	// A fully matched statement reports balanced with no differences.
	client.created("/accounts", accountBody("1200", "Savings", "asset", "CNY"))
	client.created("/journals", `{
		"id":"jv-6","date":"2024-05-04",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":40000,"reference":"T-1"},
			{"account_id":"4000","side":"credit","amount_minor":40000,"reference":"T-1"}
		]}`)
	client.created("/statements", `{
		"id":"st-2","account_id":"1200","currency":"CNY",
		"period":{"start":"2024-05-01","end":"2024-05-31"},
		"opening_balance_minor":0,"closing_balance_minor":40000,
		"lines":[{"id":"s1","date":"2024-05-04","amount_minor":40000,"reference":"T-1"}]}`)
	balanced := client.created("/reconciliations", `{"id":"rec-2","statement_id":"st-2"}`)
	if text(t, balanced, "status") != "balanced" || integer(t, balanced, "difference_minor") != 0 {
		t.Fatalf("balanced reconciliation is %v", balanced)
	}
	if len(objects(t, balanced, "differences")) != 0 {
		t.Fatalf("balanced reconciliation has differences: %v", balanced["differences"])
	}
	if integer(t, balanced, "matched_count") != 1 {
		t.Fatalf("balanced reconciliation matched %v", balanced["matched_count"])
	}
}

func TestIdempotentCreates(t *testing.T) {
	client := newClient(t)
	body := accountBody("1000", "Cash", "asset", "CNY")

	firstStatus, firstRaw, _ := client.send(request{method: http.MethodPost, path: "/accounts", key: "replay", body: body})
	secondStatus, secondRaw, _ := client.send(request{
		method: http.MethodPost, path: "/accounts", key: "replay",
		body: accountBody("1000", "Different name", "asset", "USD"),
	})
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated {
		t.Fatalf("idempotent statuses are %d and %d", firstStatus, secondStatus)
	}
	if firstRaw != secondRaw {
		t.Fatalf("replayed response %s differs from the first response %s", secondRaw, firstRaw)
	}
	accounts := objects(t, client.ok("/accounts"), "accounts")
	if len(accounts) != 1 || text(t, accounts[0], "name") != "Cash" {
		t.Fatalf("idempotency created extra accounts: %v", accounts)
	}

	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "replay", body: `{
			"id":"jv-1","date":"2024-01-15",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"1000","side":"credit","amount_minor":1}
			]}`},
		409, "conflict", "already used for another operation",
	)
}

func TestUnknownFieldsAndQueryParameters(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))

	client.expect(
		request{method: http.MethodPost, path: "/accounts", key: "extra",
			body: `{"id":"1001","name":"Cash","type":"asset","parent_id":null,"currency":"CNY","note":"hello"}`},
		400, "validation_error", "unknown field",
	)
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "extra", body: `{
			"id":"jv-1","date":"2024-01-15","memo":"x","approved":true,
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"1000","side":"credit","amount_minor":1}
			]}`},
		400, "validation_error", "unknown field",
	)
	client.expect(request{method: http.MethodGet, path: "/accounts?limit=5"}, 400, "validation_error", "unknown query parameter")
	client.expect(
		request{method: http.MethodGet, path: "/accounts/1000/balance"},
		400, "validation_error", "as_of query parameter is required",
	)
	client.expect(
		request{method: http.MethodGet, path: "/accounts/1000/balance?as_of=2024-02-30"},
		400, "validation_error", "ISO 8601",
	)
	client.expect(request{method: http.MethodGet, path: "/accounts/1000/balance?as_of=2024-01-01&to=2024-02-01"},
		400, "validation_error", "unknown query parameter")
}

func TestPeriodCloseAndAdjustments(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-jan","date":"2024-01-10",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":100000},
			{"account_id":"4000","side":"credit","amount_minor":100000}
		]}`)

	close := client.created("/period-closes", `{"id":"pc-2024-01","period":{"start":"2024-01-01","end":"2024-01-31"}}`)
	if text(t, close, "status") != "closed" || text(t, close, "closed_at") == "" {
		t.Fatalf("period close is %v", close)
	}
	if integer(t, close, "adjustment_count") != 0 || len(objects(t, close, "adjustment_journal_ids")) != 0 {
		t.Fatalf("fresh period close has adjustments: %v", close)
	}
	period, ok := close["period"].(map[string]any)
	if !ok || period["start"] != "2024-01-01" || period["end"] != "2024-01-31" {
		t.Fatalf("period is %v", close["period"])
	}
	if got := text(t, client.ok("/period-closes/pc-2024-01"), "id"); got != "pc-2024-01" {
		t.Fatalf("stored period close id is %s", got)
	}

	// Ordinary journals dated inside the closed month are rejected; other
	// dates still post.
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "closed-jan", body: `{
			"id":"jv-late","date":"2024-01-31",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":100},
				{"account_id":"4000","side":"credit","amount_minor":100}
			]}`},
		409, "conflict", "closed period rejects journal",
	)
	client.created("/journals", `{
		"id":"jv-feb","date":"2024-02-01",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":100},
			{"account_id":"4000","side":"credit","amount_minor":100}
		]}`)

	// An adjustment enters the closed month through the close itself.
	adjustment := client.created("/period-closes/pc-2024-01/adjustments", `{
		"id":"jv-adj-1","date":"2024-01-31","memo":"accrue January interest",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":5000},
			{"account_id":"4000","side":"credit","amount_minor":5000}
		]}`)
	if text(t, adjustment, "memo") != "accrue January interest" {
		t.Fatalf("adjustment journal is %v", adjustment)
	}
	if got := text(t, client.ok("/journals/jv-adj-1"), "id"); got != "jv-adj-1" {
		t.Fatalf("adjustment is not readable as a journal: %s", got)
	}
	updated := client.ok("/period-closes/pc-2024-01")
	if integer(t, updated, "adjustment_count") != 1 {
		t.Fatalf("adjustment count is %v", updated["adjustment_count"])
	}
	rawIDs, ok := updated["adjustment_journal_ids"].([]any)
	if !ok || len(rawIDs) != 1 || rawIDs[0] != "jv-adj-1" {
		t.Fatalf("adjustment journal ids are %v", updated["adjustment_journal_ids"])
	}
	balance := client.ok("/accounts/1000/balance?as_of=2024-01-31")
	if integer(t, balance, "net_minor") != 105000 {
		t.Fatalf("January balance misses the adjustment: %v", balance)
	}

	// Validation and conflict messages of the close itself.
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "bad-id",
			body: `{"id":"has space","period":{"start":"2024-03-01","end":"2024-03-31"}}`},
		400, "validation_error", "invalid period close id",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "bad-date",
			body: `{"id":"pc-bad","period":{"start":"2024-03-01","end":"2024-03-32"}}`},
		400, "validation_error", "invalid period date",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "partial",
			body: `{"id":"pc-partial","period":{"start":"2024-03-01","end":"2024-03-30"}}`},
		400, "validation_error", "period is not one calendar month",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "spanning",
			body: `{"id":"pc-span","period":{"start":"2024-03-01","end":"2024-04-30"}}`},
		400, "validation_error", "period is not one calendar month",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "dup-close",
			body: `{"id":"pc-2024-01","period":{"start":"2024-01-01","end":"2024-01-31"}}`},
		409, "conflict", "period close exists",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "same-month",
			body: `{"id":"pc-again","period":{"start":"2024-01-01","end":"2024-01-31"}}`},
		409, "conflict", "calendar month is closed",
	)
	client.expect(request{method: http.MethodGet, path: "/period-closes/pc-2024-02"}, 404, "not_found", "period close not found")

	// Adjustment validation.
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-2024-02/adjustments", key: "adj-unknown", body: `{
			"id":"jv-adj-2","date":"2024-01-15","memo":"x",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"4000","side":"credit","amount_minor":1}
			]}`},
		404, "not_found", "period close not found",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-2024-01/adjustments", key: "adj-memo", body: `{
			"id":"jv-adj-2","date":"2024-01-15",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"4000","side":"credit","amount_minor":1}
			]}`},
		400, "validation_error", "adjustment memo required",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-2024-01/adjustments", key: "adj-date", body: `{
			"id":"jv-adj-2","date":"2024-02-01","memo":"late",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"4000","side":"credit","amount_minor":1}
			]}`},
		400, "validation_error", "adjustment date outside period",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-2024-01/adjustments", key: "adj-dup", body: `{
			"id":"jv-jan","date":"2024-01-15","memo":"duplicate",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"4000","side":"credit","amount_minor":1}
			]}`},
		409, "conflict", "journal exists",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-2024-01/adjustments", key: "adj-unbalanced", body: `{
			"id":"jv-adj-2","date":"2024-01-15","memo":"unbalanced",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":100},
				{"account_id":"4000","side":"credit","amount_minor":90}
			]}`},
		400, "validation_error", "not balanced in CNY",
	)

	// Idempotency: replay returns the first response, reuse elsewhere conflicts.
	firstStatus, firstRaw, _ := client.send(request{method: http.MethodPost, path: "/period-closes", key: "pc-replay",
		body: `{"id":"pc-2024-03","period":{"start":"2024-03-01","end":"2024-03-31"}}`})
	secondStatus, secondRaw, _ := client.send(request{method: http.MethodPost, path: "/period-closes", key: "pc-replay",
		body: `{"id":"pc-2024-03","period":{"start":"2024-04-01","end":"2024-04-30"}}`})
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated || firstRaw != secondRaw {
		t.Fatalf("replay did not return the first response: %d %s then %d %s", firstStatus, firstRaw, secondStatus, secondRaw)
	}
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "pc-replay", body: `{
			"id":"jv-x","date":"2024-05-02",
			"lines":[
				{"account_id":"1000","side":"debit","amount_minor":1},
				{"account_id":"4000","side":"credit","amount_minor":1}
			]}`},
		409, "conflict", "already used for another operation",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes",
			body: `{"id":"pc-2024-06","period":{"start":"2024-06-01","end":"2024-06-30"}}`},
		400, "validation_error", "Idempotency-Key",
	)
}

func TestDatabaseSurvivesReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	client := newClientOn(t, database)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-1","date":"2024-01-15",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":4200},
			{"account_id":"4000","side":"credit","amount_minor":4200}
		]}`)

	store, err := reconcore.OpenStore(database)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	service, err := reconcore.NewService(store, "CNY", func() time.Time { return fixedClock })
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	balance, err := service.GetBalance("1000", "2024-12-31")
	if err != nil {
		t.Fatalf("balance after reopen: %v", err)
	}
	encoded, err := json.Marshal(balance)
	if err != nil {
		t.Fatalf("encode balance: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if integer(t, decoded, "balance_minor") != 4200 || integer(t, decoded, "posting_count") != 1 {
		t.Fatalf("reopened balance is %v", decoded)
	}
	if _, err := service.GetJournal("jv-1"); err != nil {
		t.Fatalf("journal is missing after reopen: %v", err)
	}
}

func TestTrialBalanceOnEmptyLedger(t *testing.T) {
	client := newClient(t)

	report := client.ok("/reports/trial-balance?as_of=2024-12-31")
	if text(t, report, "as_of") != "2024-12-31" || text(t, report, "functional_currency") != "CNY" {
		t.Fatalf("report header is %v", report)
	}
	if len(objects(t, report, "accounts")) != 0 {
		t.Fatalf("empty ledger must return no rows: %v", report["accounts"])
	}
	for _, name := range []string{
		"functional_debit_total_minor", "functional_credit_total_minor",
	} {
		if integer(t, report, name) != 0 {
			t.Fatalf("%s is %v, want zero", name, report[name])
		}
	}
	if integer(t, report, "posting_count") != 0 || text(t, report, "status") != "balanced" {
		t.Fatalf("empty report totals are %v", report)
	}
}

func TestTrialBalanceRowsAndTotals(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Assets", "asset", "CNY"))
	client.created("/accounts", accountBody("1200", "USD cash", "asset", "USD"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/accounts", accountBody("4200", "USD revenue", "revenue", "USD"))
	client.created("/accounts", accountBody("9000", "Unused", "expense", "CNY"))
	client.created("/rates", `{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.0"}`)

	client.created("/journals", `{
		"id":"jv-cny","date":"2024-01-15",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":100000},
			{"account_id":"4000","side":"credit","amount_minor":100000}
		]}`)
	client.created("/journals", `{
		"id":"jv-usd","date":"2024-02-10",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":10000},
			{"account_id":"4200","side":"credit","amount_minor":10000}
		]}`)
	// A later journal must stay out of the January as-of answer.
	client.created("/journals", `{
		"id":"jv-mar","date":"2024-03-01",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":500},
			{"account_id":"4000","side":"credit","amount_minor":500}
		]}`)

	report := client.ok("/reports/trial-balance?as_of=2024-02-29")
	if text(t, report, "as_of") != "2024-02-29" || text(t, report, "functional_currency") != "CNY" {
		t.Fatalf("report header is %v", report)
	}
	rows := objects(t, report, "accounts")
	if len(rows) != 5 {
		t.Fatalf("every account must have one row, got %v", rows)
	}
	wantIDs := []string{"1000", "1200", "4000", "4200", "9000"}
	for index, want := range wantIDs {
		if text(t, rows[index], "account_id") != want {
			t.Fatalf("row %d is %v, want %s; rows are not id-sorted", index, rows[index]["account_id"], want)
		}
	}

	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[text(t, row, "account_id")] = row
	}
	cash := byID["1000"]
	if text(t, cash, "account_name") != "Assets" || text(t, cash, "currency") != "CNY" {
		t.Fatalf("CNY cash row is %v", cash)
	}
	if integer(t, cash, "debit_minor") != 100000 || integer(t, cash, "credit_minor") != 0 ||
		integer(t, cash, "net_minor") != 100000 || integer(t, cash, "functional_net_minor") != 100000 ||
		integer(t, cash, "posting_count") != 1 {
		t.Fatalf("CNY cash totals are %v", cash)
	}
	usd := byID["1200"]
	if text(t, usd, "currency") != "USD" {
		t.Fatalf("USD row currency is %v", usd["currency"])
	}
	if integer(t, usd, "debit_minor") != 10000 || integer(t, usd, "functional_debit_minor") != 70000 ||
		integer(t, usd, "net_minor") != 10000 || integer(t, usd, "functional_net_minor") != 70000 ||
		integer(t, usd, "posting_count") != 1 {
		t.Fatalf("USD cash totals are %v", usd)
	}
	revenue := byID["4000"]
	if integer(t, revenue, "credit_minor") != 100000 || integer(t, revenue, "net_minor") != -100000 ||
		integer(t, revenue, "functional_credit_minor") != 100000 ||
		integer(t, revenue, "functional_net_minor") != -100000 {
		t.Fatalf("revenue totals are %v", revenue)
	}
	usdRevenue := byID["4200"]
	if integer(t, usdRevenue, "credit_minor") != 10000 || integer(t, usdRevenue, "functional_credit_minor") != 70000 ||
		integer(t, usdRevenue, "functional_net_minor") != -70000 {
		t.Fatalf("USD revenue totals are %v", usdRevenue)
	}
	unused := byID["9000"]
	if integer(t, unused, "debit_minor") != 0 || integer(t, unused, "credit_minor") != 0 ||
		integer(t, unused, "net_minor") != 0 || integer(t, unused, "functional_net_minor") != 0 ||
		integer(t, unused, "posting_count") != 0 {
		t.Fatalf("unused account must be all zero: %v", unused)
	}

	if integer(t, report, "functional_debit_total_minor") != 170000 ||
		integer(t, report, "functional_credit_total_minor") != 170000 {
		t.Fatalf("functional totals are %v", report)
	}
	if integer(t, report, "posting_count") != 4 || text(t, report, "status") != "balanced" {
		t.Fatalf("report footer is %v", report)
	}

	// The same as_of is recomputed after another posting; the report writes
	// nothing itself.
	later := client.ok("/reports/trial-balance?as_of=2024-12-31")
	if integer(t, later, "functional_debit_total_minor") != 170500 ||
		integer(t, later, "functional_credit_total_minor") != 170500 ||
		integer(t, later, "posting_count") != 6 {
		t.Fatalf("year-end report did not pick up the March journal: %v", later)
	}
	again := client.ok("/reports/trial-balance?as_of=2024-02-29")
	if integer(t, again, "posting_count") != 4 {
		t.Fatalf("February answer changed after re-query: %v", again)
	}
}

func TestTrialBalanceDoesNotRollUpChildren(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Assets", "asset", "CNY"))
	client.created("/accounts", `{"id":"1001","name":"Cash","type":"asset","parent_id":"1000","currency":"CNY"}`)
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-1","date":"2024-01-15",
		"lines":[
			{"account_id":"1001","side":"debit","amount_minor":100000},
			{"account_id":"4000","side":"credit","amount_minor":100000}
		]}`)

	rows := objects(t, client.ok("/reports/trial-balance?as_of=2024-01-31"), "accounts")
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[text(t, row, "account_id")] = row
	}
	if integer(t, byID["1000"], "posting_count") != 0 || integer(t, byID["1000"], "net_minor") != 0 {
		t.Fatalf("parent account rolled up its child: %v", byID["1000"])
	}
	if integer(t, byID["1001"], "debit_minor") != 100000 || integer(t, byID["1001"], "posting_count") != 1 {
		t.Fatalf("child row is %v", byID["1001"])
	}
}

func TestTrialBalanceValidationAndRouting(t *testing.T) {
	client := newClient(t)

	client.expect(
		request{method: http.MethodGet, path: "/reports/trial-balance"},
		400, "validation_error", "as_of query parameter is required",
	)
	client.expect(
		request{method: http.MethodGet, path: "/reports/trial-balance?as_of=2024-02-30"},
		400, "validation_error", "ISO 8601",
	)
	client.expect(
		request{method: http.MethodGet, path: "/reports/trial-balance?as_of=2024-01-01&as_of=2024-02-01"},
		400, "validation_error", "must appear exactly once",
	)
	client.expect(
		request{method: http.MethodGet, path: "/reports/trial-balance?as_of=2024-01-01&unknown=1"},
		400, "validation_error", "unknown query parameter",
	)
	client.expect(
		request{method: http.MethodGet, path: "/reports/trial-balance?unknown=1"},
		400, "validation_error", "unknown query parameter",
	)
	client.expect(request{method: http.MethodGet, path: "/reports/other"}, 404, "not_found", "")
	client.expect(request{method: http.MethodPost, path: "/reports/trial-balance"}, 404, "not_found", "")
}

func TestFinancialStatementsOnEmptyLedger(t *testing.T) {
	client := newClient(t)

	report := client.ok("/reports/financial-statements?as_of=2024-01-31&period_start=2024-01-01&period_end=2024-01-31")
	if text(t, report, "as_of") != "2024-01-31" ||
		text(t, report, "period_start") != "2024-01-01" ||
		text(t, report, "period_end") != "2024-01-31" ||
		text(t, report, "functional_currency") != "CNY" {
		t.Fatalf("report header is %v", report)
	}
	sheet := report["balance_sheet"].(map[string]any)
	income := report["income_statement"].(map[string]any)
	for _, column := range []string{"assets", "liabilities", "equity"} {
		if len(objects(t, sheet, column)) != 0 {
			t.Fatalf("empty balance sheet has %s rows: %v", column, sheet[column])
		}
	}
	for _, column := range []string{"revenue", "expenses"} {
		if len(objects(t, income, column)) != 0 {
			t.Fatalf("empty income statement has %s rows: %v", column, income[column])
		}
	}
	for _, name := range []string{
		"assets_total_minor", "liabilities_total_minor", "equity_total_minor",
		"balance_check_minor",
	} {
		if integer(t, sheet, name) != 0 {
			t.Fatalf("%s is %v, want zero", name, sheet[name])
		}
	}
	if text(t, sheet, "status") != "balanced" {
		t.Fatalf("empty balance sheet status is %v", sheet["status"])
	}
	for _, name := range []string{"revenue_total_minor", "expenses_total_minor", "net_income_minor"} {
		if integer(t, income, name) != 0 {
			t.Fatalf("%s is %v, want zero", name, income[name])
		}
	}
}

func TestFinancialStatementsRowsAndTotals(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Assets top", "asset", "CNY"))
	client.created("/accounts", `{"id":"1100","name":"Cash","type":"asset","parent_id":"1000","currency":"CNY"}`)
	client.created("/accounts", accountBody("1200", "USD cash", "asset", "USD"))
	client.created("/accounts", accountBody("2000", "Loan", "liability", "CNY"))
	client.created("/accounts", accountBody("3000", "Equity", "equity", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/accounts", accountBody("4200", "USD revenue", "revenue", "USD"))
	client.created("/accounts", accountBody("5000", "Rent", "expense", "CNY"))
	client.created("/accounts", accountBody("5100", "Unused", "expense", "CNY"))
	client.created("/rates", `{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.0"}`)

	// Opening balance: cash against equity.
	client.created("/journals", `{
		"id":"jv-open","date":"2024-01-01",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":1000000},
			{"account_id":"3000","side":"credit","amount_minor":1000000}
		]}`)
	// January activity inside the income period.
	client.created("/journals", `{
		"id":"jv-sale","date":"2024-01-15",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":300000},
			{"account_id":"4000","side":"credit","amount_minor":300000}
		]}`)
	client.created("/journals", `{
		"id":"jv-usd","date":"2024-01-20",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":100},
			{"account_id":"4200","side":"credit","amount_minor":100}
		]}`)
	client.created("/journals", `{
		"id":"jv-rent","date":"2024-01-31",
		"lines":[
			{"account_id":"5000","side":"debit","amount_minor":80000},
			{"account_id":"1100","side":"credit","amount_minor":80000}
		]}`)
	// February activity counts on the balance sheet but not in January income.
	client.created("/journals", `{
		"id":"jv-feb","date":"2024-02-10",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":50000},
			{"account_id":"4000","side":"credit","amount_minor":50000}
		]}`)
	client.created("/journals", `{
		"id":"jv-loan","date":"2024-02-15",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":200000},
			{"account_id":"2000","side":"credit","amount_minor":200000}
		]}`)

	report := client.ok("/reports/financial-statements?as_of=2024-02-29&period_start=2024-01-01&period_end=2024-01-31")
	sheet := report["balance_sheet"].(map[string]any)
	income := report["income_statement"].(map[string]any)

	wantOrder := func(column string, container map[string]any, ids []string) {
		t.Helper()
		rows := objects(t, container, column)
		if len(rows) != len(ids) {
			t.Fatalf("%s rows are %v, want %v", column, rows, ids)
		}
		for index, id := range ids {
			if text(t, rows[index], "account_id") != id {
				t.Fatalf("%s is not id-sorted: %v", column, rows)
			}
		}
	}
	wantOrder("assets", sheet, []string{"1000", "1100", "1200"})
	wantOrder("liabilities", sheet, []string{"2000"})
	wantOrder("equity", sheet, []string{"3000"})
	wantOrder("revenue", income, []string{"4000", "4200"})
	wantOrder("expenses", income, []string{"5000", "5100"})

	rows := map[string]map[string]any{}
	for _, column := range []string{"assets", "liabilities", "equity"} {
		for _, row := range objects(t, sheet, column) {
			rows[text(t, row, "account_id")] = row
		}
	}
	for _, column := range []string{"revenue", "expenses"} {
		for _, row := range objects(t, income, column) {
			rows[text(t, row, "account_id")] = row
		}
	}
	check := func(id string, wantBalance int64) {
		t.Helper()
		row := rows[id]
		if integer(t, row, "functional_balance_minor") != wantBalance {
			t.Fatalf("%s functional_balance_minor is %v, want %d", id, row["functional_balance_minor"], wantBalance)
		}
		if text(t, row, "account_name") == "" || text(t, row, "currency") == "" {
			t.Fatalf("%s is missing name or currency: %v", id, row)
		}
	}
	// The parent account keeps its own zero balance; children do not roll up.
	check("1000", 0)
	if rows["1000"]["parent_id"] != nil || rows["1100"]["parent_id"] != "1000" {
		t.Fatalf("parent_id values are %v and %v", rows["1000"]["parent_id"], rows["1100"]["parent_id"])
	}
	// 1000000 + 300000 - 80000 + 50000 + 200000.
	check("1100", 1470000)
	// 100 USD at 7.0 uses the stored functional amount, never a fresh conversion.
	check("1200", 700)
	if text(t, rows["1200"], "currency") != "USD" {
		t.Fatalf("USD row currency is %v", rows["1200"]["currency"])
	}
	check("2000", 200000)
	check("3000", 1000000)
	// Income rows only see January journals.
	check("4000", 300000)
	check("4200", 700)
	check("5000", 80000)
	check("5100", 0)

	if integer(t, sheet, "assets_total_minor") != 1470700 {
		t.Fatalf("assets_total_minor is %v", sheet["assets_total_minor"])
	}
	if integer(t, sheet, "liabilities_total_minor") != 200000 {
		t.Fatalf("liabilities_total_minor is %v", sheet["liabilities_total_minor"])
	}
	// Equity accounts 1000000 plus income through as_of (350700 revenue minus
	// 80000 expenses).
	if integer(t, sheet, "equity_total_minor") != 1270700 {
		t.Fatalf("equity_total_minor is %v", sheet["equity_total_minor"])
	}
	if integer(t, sheet, "balance_check_minor") != 0 || text(t, sheet, "status") != "balanced" {
		t.Fatalf("sheet footer is %v %v", sheet["balance_check_minor"], sheet["status"])
	}
	if integer(t, income, "revenue_total_minor") != 300700 ||
		integer(t, income, "expenses_total_minor") != 80000 ||
		integer(t, income, "net_income_minor") != 220700 {
		t.Fatalf("income footer is %v", income)
	}

	// The report is read-only: re-querying returns the same answer and no
	// voucher, snapshot or close was created.
	again := client.ok("/reports/financial-statements?as_of=2024-01-31&period_start=2024-01-01&period_end=2024-01-31")
	againSheet := again["balance_sheet"].(map[string]any)
	if integer(t, againSheet, "assets_total_minor") != 1220700 ||
		integer(t, againSheet, "balance_check_minor") != 0 {
		t.Fatalf("January as-of answer is %v", againSheet)
	}
	client.expect(request{method: http.MethodGet, path: "/journals/jv-report"}, 404, "not_found", "")
}

func TestFinancialStatementsValidationAndRouting(t *testing.T) {
	client := newClient(t)
	base := "/reports/financial-statements"

	client.expect(request{method: http.MethodGet, path: base},
		400, "validation_error", "as_of query parameter is required")
	client.expect(request{method: http.MethodGet, path: base + "?period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "as_of query parameter is required")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=bad&period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "ISO 8601")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31"},
		400, "validation_error", "period_start query parameter is required")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=bad&period_end=2024-01-31"},
		400, "validation_error", "ISO 8601")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01"},
		400, "validation_error", "period_end query parameter is required")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01&period_end=bad"},
		400, "validation_error", "ISO 8601")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-02-01&period_end=2024-01-31"},
		400, "validation_error", "period_start must not be after period_end")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-15&period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "period_end must not be after as_of")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31&as_of=2024-02-01&period_start=2024-01-01&period_end=2024-01-31"},
		400, "validation_error", "must appear exactly once")
	client.expect(request{method: http.MethodGet, path: base + "?as_of=2024-01-31&period_start=2024-01-01&period_end=2024-01-31&unknown=1"},
		400, "validation_error", "unknown query parameter")
	client.expect(request{method: http.MethodPost, path: base}, 404, "not_found", "")
}

// TestFinancialStatementsUnbalancedFromCorruptStore seeds a database document
// with an imbalanced journal, which the validating write API can never produce,
// to prove the balance sheet derives its status from balance_check_minor.
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

	report := client.ok("/reports/financial-statements?as_of=2024-12-31&period_start=2024-01-01&period_end=2024-12-31")
	sheet := report["balance_sheet"].(map[string]any)
	if integer(t, sheet, "assets_total_minor") != 100 {
		t.Fatalf("assets total is %v", sheet["assets_total_minor"])
	}
	// Equity is the 60 revenue credited through as_of.
	if integer(t, sheet, "equity_total_minor") != 60 {
		t.Fatalf("equity total is %v", sheet["equity_total_minor"])
	}
	if integer(t, sheet, "balance_check_minor") != 40 {
		t.Fatalf("balance check is %v", sheet["balance_check_minor"])
	}
	if text(t, sheet, "status") != "unbalanced" {
		t.Fatalf("status is %v, want unbalanced", sheet["status"])
	}
}

// TestTrialBalanceUnbalancedFromCorruptStore seeds a database document with an
// imbalanced journal, which the validating write API can never produce, to
// prove the report derives status from its own totals.
func TestTrialBalanceUnbalancedFromCorruptStore(t *testing.T) {
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

	report := client.ok("/reports/trial-balance?as_of=2024-12-31")
	if integer(t, report, "functional_debit_total_minor") != 100 ||
		integer(t, report, "functional_credit_total_minor") != 60 {
		t.Fatalf("corrupt totals are %v", report)
	}
	if text(t, report, "status") != "unbalanced" {
		t.Fatalf("status is %v, want unbalanced", report["status"])
	}
	rows := objects(t, report, "accounts")
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[text(t, row, "account_id")] = row
	}
	if integer(t, byID["4000"], "functional_net_minor") != -60 {
		t.Fatalf("revenue row is %v", byID["4000"])
	}
}

func TestJournalReversal(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1200", "USD cash", "asset", "USD"))
	client.created("/accounts", accountBody("4200", "USD revenue", "revenue", "USD"))
	client.created("/rates", `{"base":"USD","quote":"CNY","date":"2024-01-01","rate":"7.0"}`)
	client.created("/journals", `{
		"id":"jv-1","date":"2024-01-15","memo":"sale",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":100000,"reference":"INV-1"},
			{"account_id":"4200","side":"credit","amount_minor":100000,"reference":"INV-1"}
		]}`)
	_, originalBefore, _ := client.get("/journals/jv-1")
	trialBefore := client.ok("/reports/trial-balance?as_of=2024-02-28")

	// A newer rate snapshot must not change what the reversal posts.
	client.created("/rates", `{"base":"USD","quote":"CNY","date":"2024-02-01","rate":"8.0"}`)

	status, raw, reversal := client.send(request{
		method: http.MethodPost, path: "/journals/jv-1/reversals", key: "rev-1",
		body: `{"id":"jv-1-r","date":"2024-03-01","memo":"cancel the sale"}`,
	})
	if status != http.StatusCreated {
		t.Fatalf("reversal returned %d: %s", status, raw)
	}
	if text(t, reversal, "id") != "jv-1-r" || text(t, reversal, "reversal_of") != "jv-1" {
		t.Fatalf("reversal head is %v", reversal)
	}
	if text(t, reversal, "date") != "2024-03-01" || text(t, reversal, "memo") != "cancel the sale" {
		t.Fatalf("reversal head is %v", reversal)
	}
	if integer(t, reversal, "debit_functional_minor") != 700000 ||
		integer(t, reversal, "credit_functional_minor") != 700000 {
		t.Fatalf("reversal functional totals are %v", reversal)
	}
	lines := objects(t, reversal, "lines")
	if len(lines) != 2 {
		t.Fatalf("reversal lines are %v", lines)
	}
	wantSides := []string{"credit", "debit"}
	wantAccounts := []string{"1200", "4200"}
	for index, line := range lines {
		if integer(t, line, "index") != int64(index+1) ||
			text(t, line, "account_id") != wantAccounts[index] ||
			text(t, line, "side") != wantSides[index] {
			t.Fatalf("reversal line %d is %v", index+1, line)
		}
		if integer(t, line, "amount_minor") != 100000 ||
			text(t, line, "currency") != "USD" ||
			integer(t, line, "rate_numerator") != 7 ||
			integer(t, line, "rate_denominator") != 1 ||
			text(t, line, "rate_source") != "snapshot:2024-01-01" ||
			integer(t, line, "functional_amount_minor") != 700000 ||
			text(t, line, "reference") != "INV-1" {
			t.Fatalf("reversal line %d does not mirror the original: %v", index+1, line)
		}
	}

	// GET returns the same document and the original is untouched.
	stored := client.ok("/journals/jv-1-r")
	if !reflect.DeepEqual(reversal, stored) {
		t.Fatalf("stored reversal %v differs from the created one %v", stored, reversal)
	}
	_, originalAfter, originalDoc := client.get("/journals/jv-1")
	if originalAfter != originalBefore {
		t.Fatalf("original journal changed: %s then %s", originalBefore, originalAfter)
	}
	if _, found := originalDoc["reversal_of"]; found {
		t.Fatalf("original journal carries reversal_of: %v", originalDoc)
	}

	// As-of answers before the reversal date are unchanged; from the reversal
	// date onwards the pair cancels exactly in both currencies.
	before := client.ok("/accounts/1200/balance?as_of=2024-02-29")
	if integer(t, before, "net_minor") != 100000 || integer(t, before, "posting_count") != 1 {
		t.Fatalf("balance before the reversal changed: %v", before)
	}
	after := client.ok("/accounts/1200/balance?as_of=2024-03-01")
	if integer(t, after, "debit_minor") != 100000 || integer(t, after, "credit_minor") != 100000 ||
		integer(t, after, "net_minor") != 0 || integer(t, after, "functional_net_minor") != 0 ||
		integer(t, after, "posting_count") != 2 {
		t.Fatalf("balance after the reversal is %v", after)
	}
	trialAfter := client.ok("/reports/trial-balance?as_of=2024-02-28")
	if !reflect.DeepEqual(trialBefore, trialAfter) {
		t.Fatalf("trial balance before the reversal changed: %v then %v", trialBefore, trialAfter)
	}
	yearEnd := client.ok("/reports/trial-balance?as_of=2024-12-31")
	if integer(t, yearEnd, "functional_debit_total_minor") != 0 ||
		integer(t, yearEnd, "functional_credit_total_minor") != 0 ||
		text(t, yearEnd, "status") != "balanced" {
		t.Fatalf("year-end trial balance is %v", yearEnd)
	}

	// A second journal for the failure cases.
	client.created("/journals", `{
		"id":"jv-2","date":"2024-01-20",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":500},
			{"account_id":"4200","side":"credit","amount_minor":500}
		]}`)

	client.expect(request{method: http.MethodPost, path: "/journals/jv-9/reversals", key: "rev-404",
		body: `{"id":"jv-9-r","date":"2024-04-01","memo":"x"}`}, 404, "not_found", "")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-array",
		body: `[{"id":"jv-2-r"}]`}, 400, "validation_error", "must be a JSON object")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-extra",
		body: `{"id":"jv-2-r","date":"2024-04-01","memo":"x","note":"y"}`},
		400, "validation_error", "unknown field")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-noid",
		body: `{"date":"2024-04-01","memo":"x"}`}, 400, "validation_error", "journal id is required")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-nodate",
		body: `{"id":"jv-2-r","memo":"x"}`}, 400, "validation_error", "ISO 8601")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-baddate",
		body: `{"id":"jv-2-r","date":"2024-02-30","memo":"x"}`}, 400, "validation_error", "ISO 8601")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-nomemo",
		body: `{"id":"jv-2-r","date":"2024-04-01"}`}, 400, "validation_error", "memo is required")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-emptymemo",
		body: `{"id":"jv-2-r","date":"2024-04-01","memo":""}`}, 400, "validation_error", "memo is required")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-early",
		body: `{"id":"jv-2-r","date":"2024-01-19","memo":"too early"}`},
		400, "validation_error", "must not be earlier")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals",
		body: `{"id":"jv-2-r","date":"2024-04-01","memo":"x"}`}, 400, "validation_error", "Idempotency-Key")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-dup",
		body: `{"id":"jv-2","date":"2024-04-01","memo":"duplicate id"}`}, 409, "conflict", "already exists")

	// A reversal cannot be dated inside a closed month.
	client.created("/period-closes", `{"id":"pc-2024-03","period":{"start":"2024-03-01","end":"2024-03-31"}}`)
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-closed",
		body: `{"id":"jv-2-r","date":"2024-03-15","memo":"closed month"}`},
		409, "conflict", "closed period rejects journal")

	// April is open, so the reversal posts there.
	second := client.created("/journals/jv-2/reversals", `{"id":"jv-2-r","date":"2024-04-01","memo":"cancel"}`)
	if text(t, second, "reversal_of") != "jv-2" {
		t.Fatalf("second reversal is %v", second)
	}

	// A journal can be reversed only once, and a reversal cannot be reversed.
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2/reversals", key: "rev-again",
		body: `{"id":"jv-2-r2","date":"2024-04-02","memo":"again"}`}, 409, "conflict", "already reversed")
	client.expect(request{method: http.MethodPost, path: "/journals/jv-2-r/reversals", key: "rev-self",
		body: `{"id":"jv-2-rr","date":"2024-04-02","memo":"reverse the reversal"}`},
		409, "conflict", "itself a reversal")

	// Idempotent replay returns the first response; the key cannot be reused.
	client.created("/journals", `{
		"id":"jv-3","date":"2024-01-25",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":700},
			{"account_id":"4200","side":"credit","amount_minor":700}
		]}`)
	firstStatus, firstRaw, _ := client.send(request{method: http.MethodPost, path: "/journals/jv-3/reversals",
		key: "rev-replay", body: `{"id":"jv-3-r","date":"2024-04-02","memo":"cancel"}`})
	secondStatus, secondRaw, _ := client.send(request{method: http.MethodPost, path: "/journals/jv-3/reversals",
		key: "rev-replay", body: `{"id":"jv-3-r","date":"2024-05-02","memo":"different memo"}`})
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated || firstRaw != secondRaw {
		t.Fatalf("replay did not return the first response: %d %s then %d %s",
			firstStatus, firstRaw, secondStatus, secondRaw)
	}
	replayed := client.ok("/journals/jv-3-r")
	if text(t, replayed, "date") != "2024-04-02" || text(t, replayed, "memo") != "cancel" {
		t.Fatalf("replay stored a second reversal: %v", replayed)
	}
	client.expect(request{method: http.MethodPost, path: "/journals", key: "rev-replay", body: `{
		"id":"jv-4","date":"2024-05-02",
		"lines":[
			{"account_id":"1200","side":"debit","amount_minor":1},
			{"account_id":"4200","side":"credit","amount_minor":1}
		]}`}, 409, "conflict", "already used for another operation")
}

func TestJournalReversalSurvivesReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	client := newClientOn(t, database)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-1","date":"2024-01-15",
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":4200},
			{"account_id":"4000","side":"credit","amount_minor":4200}
		]}`)
	client.created("/journals/jv-1/reversals", `{"id":"jv-1-r","date":"2024-02-01","memo":"cancel"}`)

	reopened := newClientOn(t, database)
	stored := reopened.ok("/journals/jv-1-r")
	if text(t, stored, "reversal_of") != "jv-1" {
		t.Fatalf("reversal after reopen is %v", stored)
	}
	reopened.expect(request{method: http.MethodPost, path: "/journals/jv-1/reversals", key: "rev-late",
		body: `{"id":"jv-1-r2","date":"2024-02-02","memo":"again"}`}, 409, "conflict", "already reversed")
	reopened.expect(request{method: http.MethodGet, path: "/journals/jv-1-r2"}, 404, "not_found", "")
}

func TestJournalReversalAndReconciliation(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1100", "Bank", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-1","date":"2024-03-05",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":100000,"reference":"INV-1"},
			{"account_id":"4000","side":"credit","amount_minor":100000,"reference":"INV-1"}
		]}`)
	client.created("/statements", `{
		"id":"st-1","account_id":"1100","currency":"CNY",
		"period":{"start":"2024-03-01","end":"2024-03-31"},
		"opening_balance_minor":0,"closing_balance_minor":100000,
		"lines":[{"id":"s1","date":"2024-03-05","amount_minor":100000,"reference":"INV-1"}]}`)
	frozen := client.created("/reconciliations", `{"id":"rec-1","statement_id":"st-1"}`)
	if text(t, frozen, "status") != "balanced" {
		t.Fatalf("initial reconciliation is %v", frozen)
	}

	client.created("/journals/jv-1/reversals", `{"id":"jv-1-r","date":"2024-03-20","memo":"cancel"}`)

	// The frozen reconciliation does not change.
	still := client.ok("/reconciliations/rec-1")
	if text(t, still, "status") != "balanced" || integer(t, still, "ledger_line_count") != 1 {
		t.Fatalf("frozen reconciliation changed: %v", still)
	}

	// A new reconciliation sees the reversal line under the usual rules.
	fresh := client.created("/reconciliations", `{"id":"rec-2","statement_id":"st-1"}`)
	if integer(t, fresh, "ledger_line_count") != 2 || integer(t, fresh, "matched_count") != 1 {
		t.Fatalf("fresh reconciliation is %v", fresh)
	}
	if integer(t, fresh, "ledger_net_minor") != 0 || text(t, fresh, "status") != "differences_found" {
		t.Fatalf("fresh reconciliation is %v", fresh)
	}
	summary, ok := fresh["summary"].(map[string]any)
	if !ok || integer(t, summary, "missing_in_statement") != 1 {
		t.Fatalf("fresh summary is %v", fresh["summary"])
	}
	differences := objects(t, fresh, "differences")
	if len(differences) != 1 || text(t, differences[0], "ledger_line_id") != "jv-1-r#1" {
		t.Fatalf("fresh differences are %v", differences)
	}
}

// setupResolvableReconciliation creates a reconciliation with exactly two
// frozen differences: index 1 is missing_in_ledger (s2) and index 2 is
// missing_in_statement (jv-2#1).
func setupResolvableReconciliation(t *testing.T, client *client) {
	t.Helper()
	client.created("/accounts", accountBody("1100", "Bank", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-1","date":"2024-03-05",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":100000,"reference":"INV-1"},
			{"account_id":"4000","side":"credit","amount_minor":100000,"reference":"INV-1"}
		]}`)
	client.created("/journals", `{
		"id":"jv-2","date":"2024-03-15",
		"lines":[
			{"account_id":"1100","side":"credit","amount_minor":12000,"reference":"INV-5"},
			{"account_id":"4000","side":"debit","amount_minor":12000,"reference":"INV-5"}
		]}`)
	client.created("/statements", `{
		"id":"st-1","account_id":"1100","currency":"CNY",
		"period":{"start":"2024-03-01","end":"2024-03-31"},
		"opening_balance_minor":0,"closing_balance_minor":107000,
		"lines":[
			{"id":"s1","date":"2024-03-05","amount_minor":100000,"reference":"INV-1"},
			{"id":"s2","date":"2024-03-10","amount_minor":7000,"reference":"INV-2"}
		]}`)
	reconciliation := client.created("/reconciliations", `{"id":"rec-1","statement_id":"st-1"}`)
	if text(t, reconciliation, "status") != "differences_found" || len(objects(t, reconciliation, "differences")) != 2 {
		t.Fatalf("reconciliation is %v", reconciliation)
	}
}

func resolutionBody(id string, index int, disposition, reason string) string {
	return fmt.Sprintf(`{"id":%q,"difference_index":%d,"disposition":%q,"reason":%q}`,
		id, index, disposition, reason)
}

func TestResolutionLifecycle(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)

	// Review the second difference first; the listing must still sort by index.
	resolved := client.created("/reconciliations/rec-1/resolutions",
		resolutionBody("res-2", 2, "resolved", "corrected outside the system"))
	if text(t, resolved, "reconciliation_id") != "rec-1" || text(t, resolved, "id") != "res-2" {
		t.Fatalf("resolution is %v", resolved)
	}
	if integer(t, resolved, "difference_index") != 2 || text(t, resolved, "disposition") != "resolved" {
		t.Fatalf("resolution is %v", resolved)
	}
	if text(t, resolved, "reason") != "corrected outside the system" {
		t.Fatalf("resolution reason is %v", resolved["reason"])
	}
	if text(t, resolved, "created_at") != "2024-06-01T12:00:00Z" {
		t.Fatalf("resolution created_at is %v", resolved["created_at"])
	}
	snapshot, ok := resolved["difference"].(map[string]any)
	if !ok {
		t.Fatalf("resolution snapshot is %v", resolved["difference"])
	}
	if text(t, snapshot, "type") != "missing_in_statement" || text(t, snapshot, "ledger_line_id") != "jv-2#1" {
		t.Fatalf("snapshot is %v", snapshot)
	}
	if integer(t, snapshot, "difference_minor") != 12000 || integer(t, snapshot, "ledger_amount_minor") != -12000 {
		t.Fatalf("snapshot amounts are %v", snapshot)
	}

	progress := client.ok("/reconciliations/rec-1/resolutions")
	if integer(t, progress, "difference_count") != 2 || integer(t, progress, "disposed_count") != 1 {
		t.Fatalf("progress is %v", progress)
	}
	if integer(t, progress, "remaining_count") != 1 || text(t, progress, "review_status") != "pending" {
		t.Fatalf("progress is %v", progress)
	}

	accepted := client.created("/reconciliations/rec-1/resolutions",
		resolutionBody("res-1", 1, "accepted", "  timing difference is expected  "))
	if text(t, accepted, "reason") != "timing difference is expected" {
		t.Fatalf("reason was not trimmed: %q", accepted["reason"])
	}
	firstSnapshot, ok := accepted["difference"].(map[string]any)
	if !ok || text(t, firstSnapshot, "type") != "missing_in_ledger" || text(t, firstSnapshot, "statement_line_id") != "s2" {
		t.Fatalf("first snapshot is %v", accepted["difference"])
	}

	done := client.ok("/reconciliations/rec-1/resolutions")
	if text(t, done, "review_status") != "completed" || integer(t, done, "remaining_count") != 0 {
		t.Fatalf("completed review is %v", done)
	}
	if integer(t, done, "difference_count") != 2 || integer(t, done, "disposed_count") != 2 {
		t.Fatalf("completed review is %v", done)
	}
	records := objects(t, done, "records")
	if len(records) != 2 || text(t, records[0], "id") != "res-1" || text(t, records[1], "id") != "res-2" {
		t.Fatalf("records are not sorted by difference_index: %v", records)
	}
	if integer(t, records[0], "difference_index") != 1 || integer(t, records[1], "difference_index") != 2 {
		t.Fatalf("record indexes are %v", records)
	}

	// The frozen reconciliation itself is untouched by the resolutions.
	frozen := client.ok("/reconciliations/rec-1")
	if text(t, frozen, "status") != "differences_found" || len(objects(t, frozen, "differences")) != 2 {
		t.Fatalf("frozen reconciliation changed: %v", frozen)
	}
	if integer(t, frozen, "difference_minor") != 19000 {
		t.Fatalf("frozen reconciliation changed: %v", frozen)
	}
}

func TestResolutionIdempotencyAndConflicts(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)

	body := resolutionBody("res-1", 1, "accepted", "confirmed")
	firstStatus, firstRaw, _ := client.send(request{
		method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "res-replay", body: body})
	secondStatus, secondRaw, _ := client.send(request{
		method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "res-replay", body: body})
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated {
		t.Fatalf("idempotent statuses are %d and %d", firstStatus, secondStatus)
	}
	if firstRaw != secondRaw {
		t.Fatalf("replayed response %s differs from %s", secondRaw, firstRaw)
	}
	progress := client.ok("/reconciliations/rec-1/resolutions")
	if integer(t, progress, "disposed_count") != 1 {
		t.Fatalf("replay created a second resolution: %v", progress)
	}

	// The same key used for another operation conflicts.
	sharedStatus, sharedRaw, _ := client.send(request{method: http.MethodPost, path: "/accounts", key: "shared",
		body: accountBody("9999", "Other", "asset", "CNY")})
	if sharedStatus != http.StatusCreated {
		t.Fatalf("shared-key account returned %d: %s", sharedStatus, sharedRaw)
	}
	client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "shared",
		body: resolutionBody("res-9", 2, "accepted", "reuse")}, 409, "conflict", "already used for another operation")

	// A reused resolution id conflicts, as does a second disposition of the
	// same difference, and neither writes anything.
	client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "dup-id",
		body: resolutionBody("res-1", 2, "resolved", "different difference")}, 409, "conflict", "already exists")
	client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "dup-diff",
		body: resolutionBody("res-2", 1, "resolved", "second opinion")}, 409, "conflict", "already has resolution")
	progress = client.ok("/reconciliations/rec-1/resolutions")
	if integer(t, progress, "disposed_count") != 1 || len(objects(t, progress, "records")) != 1 {
		t.Fatalf("conflicts wrote records: %v", progress)
	}
}

func TestResolutionValidation(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)

	post := func(key, body string) {
		client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions",
			key: key, body: body}, 400, "validation_error", "")
	}
	post("v-1", resolutionBody("res-v1", 0, "accepted", "zero index"))
	post("v-2", resolutionBody("res-v2", -1, "accepted", "negative index"))
	post("v-3", resolutionBody("res-v3", 3, "accepted", "out of range"))
	post("v-4", `{"id":"res-v4","difference_index":1.5,"disposition":"accepted","reason":"fraction"}`)
	post("v-5", `{"id":"res-v5","difference_index":"1","disposition":"accepted","reason":"string"}`)
	post("v-6", resolutionBody("res-v6", 1, "ignored", "bad disposition"))
	post("v-7", resolutionBody("res-v7", 1, "accepted", "   "))
	post("v-8", resolutionBody("", 1, "accepted", "missing id"))
	post("v-9", `{"id":"res-v9","difference_index":1,"disposition":"accepted","reason":"x","note":"extra"}`)
	post("v-10", `{"difference_index":1,"disposition":"accepted","reason":"missing id field"}`)

	// A missing idempotency key is a validation error too.
	client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions",
		body: resolutionBody("res-v11", 1, "accepted", "no key")}, 400, "validation_error", "Idempotency-Key")

	// None of the rejected requests wrote anything.
	progress := client.ok("/reconciliations/rec-1/resolutions")
	if integer(t, progress, "disposed_count") != 0 || integer(t, progress, "remaining_count") != 2 {
		t.Fatalf("validation failures wrote records: %v", progress)
	}
	if text(t, progress, "review_status") != "pending" {
		t.Fatalf("review status is %v", progress)
	}

	// Unknown reconciliations and stray query parameters.
	client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-9/resolutions", key: "v-12",
		body: resolutionBody("res-v12", 1, "accepted", "ghost")}, 404, "not_found", "")
	client.expect(request{method: http.MethodGet, path: "/reconciliations/rec-9/resolutions"}, 404, "not_found", "")
	client.expect(request{method: http.MethodGet, path: "/reconciliations/rec-1/resolutions?status=pending"},
		400, "validation_error", "unknown query parameter")
}

func TestResolutionOnBalancedReconciliation(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1100", "Bank", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", `{
		"id":"jv-1","date":"2024-03-05",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":100000,"reference":"INV-1"},
			{"account_id":"4000","side":"credit","amount_minor":100000,"reference":"INV-1"}
		]}`)
	client.created("/statements", `{
		"id":"st-1","account_id":"1100","currency":"CNY",
		"period":{"start":"2024-03-01","end":"2024-03-31"},
		"opening_balance_minor":0,"closing_balance_minor":100000,
		"lines":[{"id":"s1","date":"2024-03-05","amount_minor":100000,"reference":"INV-1"}]}`)
	client.created("/reconciliations", `{"id":"rec-1","statement_id":"st-1"}`)

	progress := client.ok("/reconciliations/rec-1/resolutions")
	if len(objects(t, progress, "records")) != 0 {
		t.Fatalf("balanced reconciliation has records: %v", progress)
	}
	if integer(t, progress, "difference_count") != 0 || integer(t, progress, "disposed_count") != 0 ||
		integer(t, progress, "remaining_count") != 0 {
		t.Fatalf("balanced counts are %v", progress)
	}
	if text(t, progress, "review_status") != "completed" {
		t.Fatalf("balanced review status is %v", progress)
	}
	client.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "bal-1",
		body: resolutionBody("res-1", 1, "accepted", "nothing to dispose")},
		400, "validation_error", "out of range")
}

func TestResolutionsSurviveReopenAndLaterActivity(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	client := newClientOn(t, database)
	setupResolvableReconciliation(t, client)
	created := client.created("/reconciliations/rec-1/resolutions",
		resolutionBody("res-1", 1, "accepted", "kept forever"))

	// Later journals and a fresh reconciliation must not disturb the stored
	// resolution or its snapshot.
	client.created("/journals", `{
		"id":"jv-3","date":"2024-03-10",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":7000,"reference":"INV-2"},
			{"account_id":"4000","side":"credit","amount_minor":7000,"reference":"INV-2"}
		]}`)
	client.created("/reconciliations", `{"id":"rec-2","statement_id":"st-1"}`)

	reopened := newClientOn(t, database)
	progress := reopened.ok("/reconciliations/rec-1/resolutions")
	records := objects(t, progress, "records")
	if len(records) != 1 || integer(t, progress, "disposed_count") != 1 {
		t.Fatalf("resolutions after reopen are %v", progress)
	}
	if text(t, progress, "review_status") != "pending" || integer(t, progress, "remaining_count") != 1 {
		t.Fatalf("review status after reopen is %v", progress)
	}
	if !reflect.DeepEqual(records[0], created) {
		t.Fatalf("stored resolution %v differs from the created one %v", records[0], created)
	}
	snapshot, ok := records[0]["difference"].(map[string]any)
	if !ok || text(t, snapshot, "type") != "missing_in_ledger" || integer(t, snapshot, "difference_minor") != 7000 {
		t.Fatalf("snapshot after reopen is %v", records[0]["difference"])
	}

	// The resolution id and the disposed difference stay taken after a restart.
	reopened.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "late-1",
		body: resolutionBody("res-1", 2, "resolved", "recycled id")}, 409, "conflict", "already exists")
	reopened.expect(request{method: http.MethodPost, path: "/reconciliations/rec-1/resolutions", key: "late-2",
		body: resolutionBody("res-2", 1, "resolved", "second opinion")}, 409, "conflict", "already has resolution")
}

func batchJournalBody(id, date string, amount int) string {
	return fmt.Sprintf(`{
		"id":%q,"date":%q,
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":%d},
			{"account_id":"4000","side":"credit","amount_minor":%d}
		]}`, id, date, amount, amount)
}

func batchBody(id string, journals ...string) string {
	return `{"id":` + fmt.Sprintf("%q", id) + `,"journals":[` + strings.Join(journals, ",") + `]}`
}

func TestJournalBatchLifecycle(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))

	// A journal posted one by one, to compare against a batched twin.
	single := client.created("/journals", batchJournalBody("jv-single", "2024-01-15", 100000))

	body := batchBody("batch-1",
		batchJournalBody("jv-b1", "2024-01-15", 100000),
		batchJournalBody("jv-b2", "2024-01-16", 4200),
	)
	status, raw, batch := client.send(request{method: http.MethodPost, path: "/journal-batches", key: "batch-key-1", body: body})
	if status != http.StatusCreated {
		t.Fatalf("POST /journal-batches returned %d: %s", status, raw)
	}
	if text(t, batch, "id") != "batch-1" || integer(t, batch, "journal_count") != 2 {
		t.Fatalf("batch document is %v", batch)
	}
	if text(t, batch, "created_at") == "" {
		t.Fatalf("batch has no created_at: %v", batch)
	}
	ids, ok := batch["journal_ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "jv-b1" || ids[1] != "jv-b2" {
		t.Fatalf("journal_ids are %v, want [jv-b1 jv-b2] in request order", batch["journal_ids"])
	}

	// Every voucher reads back through GET /journals/{id}; the twin matches
	// the individually posted journal except for its id.
	twin := client.ok("/journals/jv-b1")
	delete(twin, "id")
	delete(single, "id")
	if !reflect.DeepEqual(twin, single) {
		t.Fatalf("batched journal %v differs from the individual one %v", twin, single)
	}
	if second := client.ok("/journals/jv-b2"); integer(t, second, "debit_functional_minor") != 4200 {
		t.Fatalf("second batched journal is %v", second)
	}

	// Batched journals feed the derived views like any other voucher.
	balance := client.ok("/accounts/1000/balance?as_of=2024-01-31")
	if integer(t, balance, "debit_minor") != 204200 || integer(t, balance, "posting_count") != 3 {
		t.Fatalf("balance including batched journals is %v", balance)
	}

	// GET returns the create document and accepts no query parameter.
	if again := client.ok("/journal-batches/batch-1"); !reflect.DeepEqual(again, batch) {
		t.Fatalf("GET batch %v differs from the create response %v", again, batch)
	}
	client.expect(request{method: http.MethodGet, path: "/journal-batches/batch-1?verbose=true"},
		400, "validation_error", "unknown query parameter")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/nope"}, 404, "not_found", "")

	// Replaying the same key returns the first response and writes nothing,
	// even when the replayed body carries a different payload for the same
	// batch id.
	replayStatus, replayRaw, _ := client.send(request{method: http.MethodPost, path: "/journal-batches", key: "batch-key-1",
		body: batchBody("batch-1",
			batchJournalBody("jv-b1", "2024-01-15", 100000),
			batchJournalBody("jv-b2", "2024-01-16", 4200),
			batchJournalBody("jv-x", "2024-01-17", 5))})
	if replayStatus != http.StatusCreated || replayRaw != raw {
		t.Fatalf("replay returned %d %s, want the first response %s", replayStatus, replayRaw, raw)
	}
	client.expect(request{method: http.MethodGet, path: "/journals/jv-x"}, 404, "not_found", "")

	// The same key on another operation conflicts.
	client.expect(request{method: http.MethodPost, path: "/journals", key: "batch-key-1",
		body: batchJournalBody("jv-y", "2024-01-18", 5)}, 409, "conflict", "already used for another operation")

	// The batch id stays taken.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "batch-again",
		body: batchBody("batch-1", batchJournalBody("jv-z", "2024-01-19", 5))},
		409, "conflict", "already exists")
}

func TestJournalBatchValidation(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	one := batchJournalBody("jv-1", "2024-01-15", 5)

	// Body shape, batch id and journals array rules.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v1", body: `[1,2]`},
		400, "validation_error", "must be a JSON object")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v2",
		body: `{"journals":[` + one + `]}`},
		400, "validation_error", "batch id is required")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v3",
		body: batchBody("bad id", one)},
		400, "validation_error", "printable ASCII")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v4", body: `{"id":"b-1"}`},
		400, "validation_error", "between 1 and 100")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v5",
		body: `{"id":"b-1","journals":"many"}`},
		400, "validation_error", "")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v6",
		body: `{"id":"b-1","journals":[]}`},
		400, "validation_error", "between 1 and 100")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v7",
		body: `{"id":"b-1","journals":[null]}`},
		400, "validation_error", "must be an object")

	// One hundred and one journals overflow the batch.
	entries := make([]string, 0, 101)
	for index := 0; index < 101; index++ {
		entries = append(entries, batchJournalBody(fmt.Sprintf("jv-%03d", index), "2024-01-15", 5))
	}
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v8",
		body: batchBody("b-big", entries...)},
		400, "validation_error", "between 1 and 100")

	// Unknown fields on the batch object and on a journal object.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v9",
		body: `{"id":"b-1","note":"x","journals":[` + one + `]}`},
		400, "validation_error", "unknown field")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v10",
		body: `{"id":"b-1","journals":[{"id":"jv-1","date":"2024-01-15","approved":true,"lines":[
			{"account_id":"1000","side":"debit","amount_minor":5},
			{"account_id":"4000","side":"credit","amount_minor":5}]}]}`},
		400, "validation_error", "unknown field")

	// Journal content errors stay validation errors.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v11",
		body: `{"id":"b-1","journals":[{"id":"jv-1","date":"2024-01-15","lines":[
			{"account_id":"1000","side":"debit","amount_minor":5},
			{"account_id":"4000","side":"credit","amount_minor":6}]}]}`},
		400, "validation_error", "not balanced")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v12",
		body: `{"id":"b-1","journals":[{"id":"jv-1","date":"2024-01-15","lines":[
			{"account_id":"9999","side":"debit","amount_minor":5},
			{"account_id":"4000","side":"credit","amount_minor":5}]}]}`},
		400, "validation_error", "does not exist")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v13",
		body: `{"id":"b-1","journals":[{"id":"jv-1","date":"2024-13-01","lines":[
			{"account_id":"1000","side":"debit","amount_minor":5},
			{"account_id":"4000","side":"credit","amount_minor":5}]}]}`},
		400, "validation_error", "ISO 8601")

	// The idempotency key is still required.
	client.expect(request{method: http.MethodPost, path: "/journal-batches",
		body: batchBody("b-1", one)},
		400, "validation_error", "Idempotency-Key")
}

func TestJournalBatchAtomicityAndConflicts(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journals", batchJournalBody("jv-existing", "2024-01-10", 100))
	client.created("/period-closes", `{"id":"pc-2024-02","period":{"start":"2024-02-01","end":"2024-02-29"}}`)

	// A batch whose second voucher is invalid writes nothing at all.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "a1",
		body: batchBody("b-bad",
			batchJournalBody("jv-ok", "2024-01-15", 5),
			`{"id":"jv-bad","date":"2024-01-16","lines":[
				{"account_id":"1000","side":"debit","amount_minor":5},
				{"account_id":"4000","side":"credit","amount_minor":6}]}`)},
		400, "validation_error", "not balanced")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/b-bad"}, 404, "not_found", "")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-ok"}, 404, "not_found", "")

	// A journal id that already exists conflicts and writes nothing.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "a2",
		body: batchBody("b-dup",
			batchJournalBody("jv-new", "2024-01-15", 5),
			batchJournalBody("jv-existing", "2024-01-16", 5))},
		409, "conflict", "already exists")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/b-dup"}, 404, "not_found", "")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-new"}, 404, "not_found", "")

	// A duplicate id inside the batch conflicts.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "a3",
		body: batchBody("b-inner",
			batchJournalBody("jv-twice", "2024-01-15", 5),
			batchJournalBody("jv-twice", "2024-01-16", 5))},
		409, "conflict", "duplicated")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-twice"}, 404, "not_found", "")

	// A voucher dated in a closed month conflicts and writes nothing.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "a4",
		body: batchBody("b-closed", batchJournalBody("jv-feb", "2024-02-10", 5))},
		409, "conflict", "closed period rejects journal")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/b-closed"}, 404, "not_found", "")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-feb"}, 404, "not_found", "")

	// A failed request stores no idempotency record: the same key is free once
	// the body is fixed.
	status, raw, _ := client.send(request{method: http.MethodPost, path: "/journal-batches", key: "a4",
		body: batchBody("b-closed", batchJournalBody("jv-feb", "2024-03-10", 5))})
	if status != http.StatusCreated {
		t.Fatalf("reused key after a failure returned %d: %s", status, raw)
	}
}

func TestJournalBatchSurvivesReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	client := newClientOn(t, database)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	created := client.created("/journal-batches", batchBody("batch-1",
		batchJournalBody("jv-b1", "2024-01-15", 700),
		batchJournalBody("jv-b2", "2024-01-16", 900),
	))

	reopened := newClientOn(t, database)
	if again := reopened.ok("/journal-batches/batch-1"); !reflect.DeepEqual(again, created) {
		t.Fatalf("batch after reopen %v differs from the created one %v", again, created)
	}
	if journal := reopened.ok("/journals/jv-b1"); integer(t, journal, "debit_functional_minor") != 700 {
		t.Fatalf("batched journal after reopen is %v", journal)
	}

	// The batch id stays taken after a restart.
	reopened.expect(request{method: http.MethodPost, path: "/journal-batches", key: "late",
		body: batchBody("batch-1", batchJournalBody("jv-b3", "2024-01-17", 5))},
		409, "conflict", "already exists")

	// Later activity never rewrites the committed batch record. The reopened
	// client uses fresh explicit keys because its automatic ones were already
	// spent on this database before the restart.
	for _, step := range []request{
		{method: http.MethodPost, path: "/journals", key: "late-journal",
			body: batchJournalBody("jv-later", "2024-01-20", 5)},
		{method: http.MethodPost, path: "/period-closes", key: "late-close",
			body: `{"id":"pc-2024-01","period":{"start":"2024-01-01","end":"2024-01-31"}}`},
	} {
		if status, raw, _ := reopened.send(step); status != http.StatusCreated {
			t.Fatalf("%s %s returned %d: %s", step.method, step.path, status, raw)
		}
	}
	if after := reopened.ok("/journal-batches/batch-1"); !reflect.DeepEqual(after, created) {
		t.Fatalf("batch changed after later activity: %v", after)
	}
}
