package tests

import (
	"fmt"
	"net/http"
	"testing"
)

func periodCloseBody(id, start, end string) string {
	return fmt.Sprintf(`{"id":%q,"period":{"start":%q,"end":%q}}`, id, start, end)
}

func adjustmentBody(id, date, memo string, lines string) string {
	if memo == "" {
		return fmt.Sprintf(`{"id":%q,"date":%q,"lines":%s}`, id, date, lines)
	}
	return fmt.Sprintf(`{"id":%q,"date":%q,"memo":%q,"lines":%s}`, id, date, memo, lines)
}

func cnyPair(debitAccount, creditAccount string, debit, credit int64) string {
	return fmt.Sprintf(
		`[{"account_id":%q,"side":"debit","amount_minor":%d},`+
			`{"account_id":%q,"side":"credit","amount_minor":%d}]`,
		debitAccount, debit, creditAccount, credit)
}

func stringArray(t *testing.T, container map[string]any, name string) []string {
	t.Helper()
	raw, found := container[name]
	if !found {
		t.Fatalf("field %s is missing in %v", name, container)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("field %s is %v, want an array", name, raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			t.Fatalf("field %s contains %v, want a string", name, item)
		}
		out = append(out, value)
	}
	return out
}

func TestPeriodCloseLifecycle(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))

	close := client.created("/period-closes", periodCloseBody("pc-1", "2024-02-01", "2024-02-29"))
	if text(t, close, "id") != "pc-1" {
		t.Fatalf("close id is %v", close["id"])
	}
	period, ok := close["period"].(map[string]any)
	if !ok || period["start"] != "2024-02-01" || period["end"] != "2024-02-29" {
		t.Fatalf("close period is %v", close["period"])
	}
	if text(t, close, "status") != "closed" {
		t.Fatalf("status is %v", close["status"])
	}
	if text(t, close, "closed_at") != "2024-06-01T12:00:00Z" {
		t.Fatalf("closed_at is %v", close["closed_at"])
	}
	if integer(t, close, "adjustment_count") != 0 {
		t.Fatalf("initial adjustment count is %v", close["adjustment_count"])
	}
	if ids := objects(t, close, "adjustment_journal_ids"); len(ids) != 0 {
		t.Fatalf("initial adjustment list is %v", ids)
	}

	stored := client.ok("/period-closes/pc-1")
	if text(t, stored, "status") != "closed" {
		t.Fatalf("stored close is %v", stored)
	}

	first := client.created("/period-closes/pc-1/adjustments",
		adjustmentBody("adj-1", "2024-02-15", "accrue missing fee", cnyPair("1000", "4000", 1000, 1000)))
	if text(t, first, "id") != "adj-1" || text(t, first, "memo") != "accrue missing fee" {
		t.Fatalf("adjustment response is %v", first)
	}
	if got := text(t, client.ok("/journals/adj-1"), "id"); got != "adj-1" {
		t.Fatalf("adjustment journal is not readable through GET /journals: %s", got)
	}

	afterFirst := client.ok("/period-closes/pc-1")
	if integer(t, afterFirst, "adjustment_count") != 1 {
		t.Fatalf("count after first adjustment is %v", afterFirst["adjustment_count"])
	}
	if got := stringArray(t, afterFirst, "adjustment_journal_ids"); len(got) != 1 || got[0] != "adj-1" {
		t.Fatalf("adjustment list is %v", got)
	}

	client.created("/period-closes/pc-1/adjustments",
		adjustmentBody("adj-2", "2024-02-29", "second fix", cnyPair("1000", "4000", 250, 250)))
	afterSecond := client.ok("/period-closes/pc-1")
	if integer(t, afterSecond, "adjustment_count") != 2 {
		t.Fatalf("count after second adjustment is %v", afterSecond["adjustment_count"])
	}
	if got := stringArray(t, afterSecond, "adjustment_journal_ids"); len(got) != 2 || got[0] != "adj-1" || got[1] != "adj-2" {
		t.Fatalf("adjustment list does not keep insertion order: %v", got)
	}

	// Adjustments are ordinary ledgers postings for balance derivation.
	balance := client.ok("/accounts/1000/balance?as_of=2024-02-29")
	if integer(t, balance, "debit_minor") != 1250 || integer(t, balance, "posting_count") != 2 {
		t.Fatalf("adjusted balance is %v", balance)
	}
}

func TestPeriodCloseValidation(t *testing.T) {
	client := newClient(t)

	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "bad-id",
			body: periodCloseBody("bad id", "2024-02-01", "2024-02-29")},
		400, "validation_error", "invalid period close id",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "missing-id",
			body: periodCloseBody("", "2024-02-01", "2024-02-29")},
		400, "validation_error", "invalid period close id",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "bad-start",
			body: periodCloseBody("pc-x", "2024-02-xx", "2024-02-29")},
		400, "validation_error", "invalid period date",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "missing-start",
			body: periodCloseBody("pc-x", "", "2024-02-29")},
		400, "validation_error", "invalid period date",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "bad-end",
			body: periodCloseBody("pc-x", "2024-02-01", "2024-02-30")},
		400, "validation_error", "invalid period date",
	)

	for name, body := range map[string]string{
		"short month":      periodCloseBody("pc-x", "2024-02-01", "2024-02-28"),
		"ends early":       periodCloseBody("pc-x", "2024-01-01", "2024-01-30"),
		"starts late":      periodCloseBody("pc-x", "2024-01-02", "2024-01-31"),
		"spans two months": periodCloseBody("pc-x", "2024-01-01", "2024-02-29"),
	} {
		client.expect(
			request{method: http.MethodPost, path: "/period-closes", key: "not-month-" + name, body: body},
			400, "validation_error", "period is not one calendar month",
		)
	}

	client.created("/period-closes", periodCloseBody("pc-1", "2024-02-01", "2024-02-29"))
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "dup-id",
			body: periodCloseBody("pc-1", "2024-03-01", "2024-03-31")},
		409, "conflict", "period close exists",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes", key: "dup-month",
			body: periodCloseBody("pc-2", "2024-02-01", "2024-02-29")},
		409, "conflict", "calendar month is closed",
	)

	// A different calendar month closes with its own record.
	client.created("/period-closes", periodCloseBody("pc-3", "2024-03-01", "2024-03-31"))

	client.expect(request{method: http.MethodGet, path: "/period-closes/missing"},
		404, "not_found", "period close not found")
}

func TestClosedPeriodRejectsOrdinaryJournals(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/period-closes", periodCloseBody("pc-1", "2024-02-01", "2024-02-29"))

	for _, date := range []string{"2024-02-01", "2024-02-15", "2024-02-29"} {
		client.expect(
			request{method: http.MethodPost, path: "/journals", key: "closed-" + date,
				body: fmt.Sprintf(`{"id":"jv-%s","date":%q,"lines":%s}`, date, date,
					cnyPair("1000", "4000", 100, 100))},
			409, "conflict", "closed period rejects journal",
		)
	}

	// Dates outside the closed month keep posting normally, both ends included.
	client.created("/journals",
		adjustmentBody("jv-jan", "2024-01-31", "", cnyPair("1000", "4000", 100, 100)))
	client.created("/journals",
		adjustmentBody("jv-mar", "2024-03-01", "", cnyPair("1000", "4000", 100, 100)))
}

func TestAdjustmentValidation(t *testing.T) {
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/accounts", accountBody("1200", "USD cash", "asset", "USD"))
	client.created("/period-closes", periodCloseBody("pc-1", "2024-02-01", "2024-02-29"))

	validLines := cnyPair("1000", "4000", 1000, 1000)

	client.expect(
		request{method: http.MethodPost, path: "/period-closes/missing/adjustments", key: "no-close",
			body: adjustmentBody("adj-x", "2024-02-15", "fix", validLines)},
		404, "not_found", "period close not found",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "no-memo",
			body: adjustmentBody("adj-x", "2024-02-15", "", validLines)},
		400, "validation_error", "adjustment memo required",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "empty-memo",
			body: fmt.Sprintf(`{"id":"adj-x","date":"2024-02-15","memo":"","lines":%s}`, validLines)},
		400, "validation_error", "adjustment memo required",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "early-date",
			body: adjustmentBody("adj-x", "2024-01-31", "fix", validLines)},
		400, "validation_error", "adjustment date outside period",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "late-date",
			body: adjustmentBody("adj-x", "2024-03-01", "fix", validLines)},
		400, "validation_error", "adjustment date outside period",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "single-line",
			body: adjustmentBody("adj-x", "2024-02-15", "fix",
				`[{"account_id":"1000","side":"debit","amount_minor":1000}]`)},
		400, "validation_error", "at least two postings",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "unbalanced",
			body: adjustmentBody("adj-x", "2024-02-15", "fix", cnyPair("1000", "4000", 1000, 900))},
		400, "validation_error", "not balanced in CNY",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "unknown-account",
			body: adjustmentBody("adj-x", "2024-02-15", "fix", cnyPair("9999", "4000", 100, 100))},
		400, "validation_error", "does not exist",
	)
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "no-rate",
			body: adjustmentBody("adj-x", "2024-02-15", "fix",
				`[{"account_id":"1200","side":"debit","amount_minor":100000},`+
					`{"account_id":"4000","side":"credit","amount_minor":724500}]`)},
		400, "validation_error", "no USD/CNY exchange rate snapshot",
	)

	// Nothing failed above registered an adjustment.
	if got := integer(t, client.ok("/period-closes/pc-1"), "adjustment_count"); got != 0 {
		t.Fatalf("failed adjustments changed the count: %d", got)
	}

	client.created("/period-closes/pc-1/adjustments",
		adjustmentBody("adj-1", "2024-02-15", "fix", validLines))
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "dup-journal",
			body: adjustmentBody("adj-1", "2024-02-16", "fix again", validLines)},
		409, "conflict", "journal exists",
	)
}

func TestPeriodCloseIdempotency(t *testing.T) {
	client := newClient(t)
	body := periodCloseBody("pc-1", "2024-02-01", "2024-02-29")

	client.expect(
		request{method: http.MethodPost, path: "/period-closes", body: body},
		400, "validation_error", "Idempotency-Key",
	)

	firstStatus, firstRaw, _ := client.send(request{
		method: http.MethodPost, path: "/period-closes", key: "close-replay", body: body,
	})
	secondStatus, secondRaw, _ := client.send(request{
		method: http.MethodPost, path: "/period-closes", key: "close-replay",
		body: periodCloseBody("pc-1", "2024-03-01", "2024-03-31"),
	})
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated {
		t.Fatalf("replayed statuses are %d and %d", firstStatus, secondStatus)
	}
	if firstRaw != secondRaw {
		t.Fatalf("replayed close %s differs from %s", secondRaw, firstRaw)
	}
	// The replayed body must not have closed March.
	client.ok("/period-closes/pc-1")
	client.expect(request{method: http.MethodGet, path: "/period-closes/pc-other"},
		404, "not_found", "period close not found")

	// A key reused for another operation conflicts for the new endpoints too.
	client.expect(
		request{method: http.MethodPost, path: "/journals", key: "close-replay",
			body: adjustmentBody("jv-1", "2024-01-15", "", cnyPair("1000", "4000", 100, 100))},
		409, "conflict", "already used for another operation",
	)

	// Adjustment replays return the original journal and do not double-count.
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	adjustment := adjustmentBody("adj-1", "2024-02-15", "fix",
		cnyPair("1000", "4000", 1000, 1000))
	statusA, rawA, _ := client.send(request{
		method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "adj-replay", body: adjustment,
	})
	statusB, rawB, _ := client.send(request{
		method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "adj-replay", body: adjustment,
	})
	if statusA != http.StatusCreated || statusB != http.StatusCreated || rawA != rawB {
		t.Fatalf("adjustment replay failed: %d %d %s %s", statusA, statusB, rawA, rawB)
	}
	if got := integer(t, client.ok("/period-closes/pc-1"), "adjustment_count"); got != 1 {
		t.Fatalf("replayed adjustment was counted twice: %d", got)
	}

	// Reusing the close's key for an adjustment conflicts.
	client.expect(
		request{method: http.MethodPost, path: "/period-closes/pc-1/adjustments", key: "close-replay",
			body: adjustmentBody("adj-2", "2024-02-15", "fix",
				cnyPair("1000", "4000", 100, 100))},
		409, "conflict", "already used for another operation",
	)
}
