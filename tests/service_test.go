package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
