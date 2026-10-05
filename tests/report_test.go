package tests

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

func reportBody(id, reconciliationID string) string {
	return fmt.Sprintf(`{"id":%q,"reconciliation_id":%q}`, id, reconciliationID)
}

// completeReview disposes every frozen difference of rec-1, alternating
// accepted and resolved, and returns the stored resolutions in index order.
func completeReview(t *testing.T, client *client) []map[string]any {
	t.Helper()
	progress := client.ok("/reconciliations/rec-1/resolutions")
	count := integer(t, progress, "difference_count")
	created := []map[string]any{}
	for index := int64(1); index <= count; index++ {
		disposition := "accepted"
		if index%2 == 0 {
			disposition = "resolved"
		}
		resolution := client.created("/reconciliations/rec-1/resolutions",
			resolutionBody(fmt.Sprintf("res-%d", index), int(index), disposition, "reviewed"))
		created = append(created, resolution)
	}
	done := client.ok("/reconciliations/rec-1/resolutions")
	if text(t, done, "review_status") != "completed" {
		t.Fatalf("review is not completed: %v", done)
	}
	return created
}

func TestReconciliationReportLifecycle(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)
	resolutions := completeReview(t, client)
	reconciliation := client.ok("/reconciliations/rec-1")

	report := client.created("/reconciliation-reports", reportBody("rpt-1", "rec-1"))
	if text(t, report, "id") != "rpt-1" || text(t, report, "reconciliation_id") != "rec-1" {
		t.Fatalf("report identity is %v", report)
	}
	if text(t, report, "generated_at") != "2024-06-01T12:00:00Z" {
		t.Fatalf("generated_at is %v", report["generated_at"])
	}
	if text(t, report, "review_status") != "completed" {
		t.Fatalf("review_status is %v", report["review_status"])
	}
	snapshot, ok := report["reconciliation"].(map[string]any)
	if !ok {
		t.Fatalf("reconciliation snapshot is %v", report["reconciliation"])
	}
	if !reflect.DeepEqual(snapshot, reconciliation) {
		t.Fatalf("reconciliation snapshot %v differs from the frozen reconciliation %v", snapshot, reconciliation)
	}
	records := objects(t, report, "resolutions")
	if len(records) != 2 {
		t.Fatalf("report resolutions are %v", records)
	}
	if integer(t, records[0], "difference_index") != 1 || integer(t, records[1], "difference_index") != 2 {
		t.Fatalf("report resolutions are not ordered by difference_index: %v", records)
	}
	if !reflect.DeepEqual(records[0], resolutions[0]) || !reflect.DeepEqual(records[1], resolutions[1]) {
		t.Fatalf("report resolution snapshots differ from the stored resolutions")
	}
	summary, ok := report["disposition_summary"].(map[string]any)
	if !ok {
		t.Fatalf("disposition_summary is %v", report["disposition_summary"])
	}
	if integer(t, summary, "accepted_count") != 1 || integer(t, summary, "resolved_count") != 1 ||
		integer(t, summary, "total_count") != 2 {
		t.Fatalf("disposition_summary is %v", summary)
	}

	// GET returns the same document published at creation time.
	fetched := client.ok("/reconciliation-reports/rpt-1")
	if !reflect.DeepEqual(fetched, report) {
		t.Fatalf("fetched report %v differs from the created one %v", fetched, report)
	}
}

func TestReconciliationReportOnBalancedReconciliation(t *testing.T) {
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
	reconciliation := client.created("/reconciliations", `{"id":"rec-1","statement_id":"st-1"}`)
	if text(t, reconciliation, "status") != "balanced" {
		t.Fatalf("reconciliation is %v", reconciliation)
	}

	report := client.created("/reconciliation-reports", reportBody("rpt-1", "rec-1"))
	if text(t, report, "review_status") != "completed" {
		t.Fatalf("balanced report review_status is %v", report["review_status"])
	}
	if len(objects(t, report, "resolutions")) != 0 {
		t.Fatalf("balanced report carries resolutions: %v", report["resolutions"])
	}
	summary, ok := report["disposition_summary"].(map[string]any)
	if !ok {
		t.Fatalf("disposition_summary is %v", report["disposition_summary"])
	}
	if integer(t, summary, "accepted_count") != 0 || integer(t, summary, "resolved_count") != 0 ||
		integer(t, summary, "total_count") != 0 {
		t.Fatalf("balanced disposition_summary is %v", summary)
	}
}

func TestReconciliationReportConflicts(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)

	// An unfinished review cannot be published.
	client.created("/reconciliations/rec-1/resolutions",
		resolutionBody("res-1", 1, "accepted", "one of two"))
	client.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "pending-1",
		body: reportBody("rpt-pending", "rec-1")}, 409, "conflict", "undisposed differences")

	// An unknown reconciliation is a 404, even though its body is otherwise valid.
	client.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "missing-1",
		body: reportBody("rpt-missing", "rec-9")}, 404, "not_found", "")

	client.created("/reconciliations/rec-1/resolutions",
		resolutionBody("res-2", 2, "resolved", "handled outside"))
	done := client.ok("/reconciliations/rec-1/resolutions")
	if text(t, done, "review_status") != "completed" {
		t.Fatalf("review is not completed: %v", done)
	}
	client.created("/reconciliation-reports", reportBody("rpt-1", "rec-1"))

	// The report id is taken.
	client.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "dup-id",
		body: reportBody("rpt-1", "rec-1")}, 409, "conflict", "already exists")

	// One reconciliation can be reported only once, even under a new report id.
	client.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "dup-recon",
		body: reportBody("rpt-2", "rec-1")}, 409, "conflict", "already has report")

	// The failed attempts did not produce additional reports.
	client.expect(request{method: http.MethodGet, path: "/reconciliation-reports/rpt-2"},
		404, "not_found", "")
}

func TestReconciliationReportValidation(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)
	completeReview(t, client)

	post := func(key, body string) {
		client.expect(request{method: http.MethodPost, path: "/reconciliation-reports",
			key: key, body: body}, 400, "validation_error", "")
	}
	post("v-1", `[1,2]`)
	post("v-2", `"nope"`)
	post("v-3", `{}`)
	post("v-4", `{"id":"rpt-v4","reconciliation_id":"rec-1","extra":true}`)
	post("v-5", `{"reconciliation_id":"rec-1"}`)
	post("v-6", `{"id":"","reconciliation_id":"rec-1"}`)
	post("v-7", `{"id":"bad id","reconciliation_id":"rec-1"}`)
	post("v-8", `{"id":"rpt-v8","reconciliation_id":""}`)
	post("v-9", `{"id":"rpt-v9","reconciliation_id":"rec 1"}`)
	post("v-10", `{"id":"rpt-v10","reconciliation_id":"rec-1"} trailing`)

	// A missing idempotency key is a validation error as well.
	client.expect(request{method: http.MethodPost, path: "/reconciliation-reports",
		body: reportBody("rpt-v11", "rec-1")}, 400, "validation_error", "Idempotency-Key")

	// None of the rejected requests wrote anything.
	client.expect(request{method: http.MethodGet, path: "/reconciliation-reports/rpt-v4"},
		404, "not_found", "")

	// Unknown reports and stray query parameters.
	client.expect(request{method: http.MethodGet, path: "/reconciliation-reports/rpt-ghost"},
		404, "not_found", "")
	client.expect(request{method: http.MethodGet, path: "/reconciliation-reports/rpt-1?format=pdf"},
		400, "validation_error", "unknown query parameter")
}

func TestReconciliationReportIdempotency(t *testing.T) {
	client := newClient(t)
	setupResolvableReconciliation(t, client)
	completeReview(t, client)
	body := reportBody("rpt-1", "rec-1")

	firstStatus, firstRaw, _ := client.send(request{
		method: http.MethodPost, path: "/reconciliation-reports", key: "report-replay", body: body})
	secondStatus, secondRaw, _ := client.send(request{
		method: http.MethodPost, path: "/reconciliation-reports", key: "report-replay", body: body})
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated {
		t.Fatalf("idempotent statuses are %d and %d", firstStatus, secondStatus)
	}
	if firstRaw != secondRaw {
		t.Fatalf("replayed report %s differs from %s", secondRaw, firstRaw)
	}

	// The same key used for another operation conflicts and writes nothing.
	client.expect(request{method: http.MethodPost, path: "/accounts", key: "report-replay",
		body: accountBody("9999", "Other", "asset", "CNY")}, 409, "conflict", "already used for another operation")
	client.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "other-key",
		body: reportBody("rpt-2", "rec-1")}, 409, "conflict", "already has report")
}

func TestReconciliationReportSurvivesReopenAndLaterActivity(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	client := newClientOn(t, database)
	setupResolvableReconciliation(t, client)
	completeReview(t, client)
	firstStatus, firstBody, firstReport := client.send(request{
		method: http.MethodPost, path: "/reconciliation-reports",
		key: "rpt-freeze", body: reportBody("rpt-1", "rec-1")})
	if firstStatus != http.StatusCreated {
		t.Fatalf("publish returned %d: %s", firstStatus, firstBody)
	}

	// Later journals, a fresh reconciliation and even new dispositions of other
	// reconciliations must never touch the published snapshot or its summary.
	client.created("/journals", `{
		"id":"jv-3","date":"2024-03-10",
		"lines":[
			{"account_id":"1100","side":"debit","amount_minor":7000,"reference":"INV-2"},
			{"account_id":"4000","side":"credit","amount_minor":7000,"reference":"INV-2"}
		]}`)
	client.created("/reconciliations", `{"id":"rec-2","statement_id":"st-1"}`)

	reopened := newClientOn(t, database)
	status, raw, decoded := reopened.get("/reconciliation-reports/rpt-1")
	if status != http.StatusOK {
		t.Fatalf("GET after reopen returned %d: %s", status, raw)
	}
	if !reflect.DeepEqual(decoded, firstReport) {
		t.Fatalf("report after reopen %v differs from the published one %v", decoded, firstReport)
	}
	if text(t, decoded, "review_status") != "completed" {
		t.Fatalf("report review_status after reopen is %v", decoded["review_status"])
	}
	summary, ok := decoded["disposition_summary"].(map[string]any)
	if !ok || integer(t, summary, "total_count") != 2 {
		t.Fatalf("report summary changed after reopen: %v", decoded["disposition_summary"])
	}

	// The one-report-per-reconciliation and report-id uniqueness rules hold
	// across a restart.
	reopened.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "again-1",
		body: reportBody("rpt-1", "rec-1")}, 409, "conflict", "already exists")
	reopened.expect(request{method: http.MethodPost, path: "/reconciliation-reports", key: "again-2",
		body: reportBody("rpt-2", "rec-1")}, 409, "conflict", "already has report")

	// Replaying the original idempotency key after restart returns the first
	// document without creating a new report.
	replayStatus, replayRaw, _ := reopened.send(request{
		method: http.MethodPost, path: "/reconciliation-reports",
		key: "rpt-freeze", body: reportBody("rpt-1", "rec-1")})
	if replayStatus != http.StatusCreated || replayRaw != firstBody {
		t.Fatalf("replay after restart gave %d %s, want the original 201 document", replayStatus, replayRaw)
	}
}
