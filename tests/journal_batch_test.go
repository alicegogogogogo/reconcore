package tests

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// batchJournalBody renders one ordinary journal request for batch tests.
func batchJournalBody(id, date string, amount int64) string {
	return fmt.Sprintf(`{
		"id":%q,"date":%q,
		"lines":[
			{"account_id":"1000","side":"debit","amount_minor":%d},
			{"account_id":"4000","side":"credit","amount_minor":%d}
		]}`, id, date, amount, amount)
}

func newBatchClient(t *testing.T) *client {
	t.Helper()
	client := newClient(t)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	return client
}

func TestJournalBatchLifecycle(t *testing.T) {
	client := newBatchClient(t)

	status, raw, batch := client.send(request{
		method: http.MethodPost, path: "/journal-batches", key: "batch-1",
		body: `{"id":"batch-1","journals":[` +
			batchJournalBody("jv-a", "2024-01-10", 1000) + `,` +
			batchJournalBody("jv-b", "2024-01-11", 2000) + `]}`,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /journal-batches returned %d: %s", status, raw)
	}
	if text(t, batch, "id") != "batch-1" {
		t.Fatalf("batch id is %v", batch["id"])
	}
	if integer(t, batch, "journal_count") != 2 {
		t.Fatalf("journal_count is %v", batch["journal_count"])
	}
	if text(t, batch, "created_at") != "2024-06-01T12:00:00Z" {
		t.Fatalf("created_at is %v", batch["created_at"])
	}
	ids := batch["journal_ids"].([]any)
	if len(ids) != 2 || ids[0] != "jv-a" || ids[1] != "jv-b" {
		t.Fatalf("journal_ids are not in request order: %v", ids)
	}
	if len(batch) != 4 {
		t.Fatalf("batch document carries unexpected fields: %v", batch)
	}

	// The committed batch is readable and carries the same document.
	_, _, fetched := client.send(request{method: http.MethodGet, path: "/journal-batches/batch-1"})
	if !reflect.DeepEqual(fetched, batch) {
		t.Fatalf("GET /journal-batches/batch-1 = %v, want %v", fetched, batch)
	}
	if text(t, fetched, "id") != "batch-1" {
		t.Fatalf("fetched batch id is %v", fetched["id"])
	}

	// Every journal is readable on its own and matches a single-post voucher.
	journal := client.ok("/journals/jv-a")
	if text(t, journal, "date") != "2024-01-10" || integer(t, journal, "debit_functional_minor") != 1000 {
		t.Fatalf("journal jv-a does not match its request: %v", journal)
	}
	if integer(t, client.ok("/journals/jv-b"), "credit_functional_minor") != 2000 {
		t.Fatalf("journal jv-b does not match its request: %v", journal)
	}

	client.expect(request{method: http.MethodGet, path: "/journal-batches/nope"},
		404, "not_found", "not found")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/batch-1?verbose=true"},
		400, "validation_error", "unknown query parameter")
}

func TestJournalBatchValidation(t *testing.T) {
	client := newBatchClient(t)

	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v1", body: `[1,2]`},
		400, "validation_error", "must be a JSON object")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v2",
		body: `{"journals":[]}`},
		400, "validation_error", "batch id is required")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v3",
		body: `{"id":"has space","journals":[]}`},
		400, "validation_error", "batch id")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v4",
		body: `{"id":"b1"}`},
		400, "validation_error", "journals is required")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v5",
		body: `{"id":"b1","journals":"soon"}`},
		400, "validation_error", "does not match the documented schema")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v6",
		body: `{"id":"b1","journals":[]}`},
		400, "validation_error", "between 1 and 100")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v7",
		body: `{"id":"b1","journals":[` + strings.Repeat(batchJournalBody("jv-x", "2024-01-10", 1)+`,`, 100) + batchJournalBody("jv-y", "2024-01-10", 1) + `]}`},
		400, "validation_error", "between 1 and 100")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v8",
		body: `{"id":"b1","journals":[` + batchJournalBody("jv-a", "2024-01-10", 1) + `],"extra":1}`},
		400, "validation_error", "unknown field")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v9",
		body: `{"id":"b1","journals":[{"id":"jv-a","date":"2024-01-10","bogus":1,"lines":[]}]}`},
		400, "validation_error", "unknown field")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v10",
		body: `{"id":"b1","journals":[null]}`},
		400, "validation_error", "must be an object")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v11",
		body: `{"id":"b1","journals":[{"id":"jv-a","date":"2024-01-10","lines":[
			{"account_id":"1000","side":"debit","amount_minor":100},
			{"account_id":"4000","side":"credit","amount_minor":90}
		]}]}`},
		400, "validation_error", "not balanced in CNY")
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "v12",
		body: `{"id":"b1","journals":[{"id":"jv-a","date":"2024-01-10","lines":[
			{"account_id":"1000","side":"debit","amount_minor":100}
		]}]}`},
		400, "validation_error", "at least two postings")
	// Missing Idempotency-Key is rejected like every other write.
	client.expect(request{method: http.MethodPost, path: "/journal-batches",
		body: `{"id":"b1","journals":[` + batchJournalBody("jv-a", "2024-01-10", 100) + `]}`},
		400, "validation_error", "Idempotency-Key")

	// None of the failed attempts wrote anything.
	client.expect(request{method: http.MethodGet, path: "/journal-batches/b1"},
		404, "not_found", "not found")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-a"},
		404, "not_found", "not found")
}

func TestJournalBatchConflictsAndAtomicity(t *testing.T) {
	client := newBatchClient(t)
	client.created("/journals", batchJournalBody("jv-existing", "2024-01-05", 500))

	// A journal id that already exists rejects the whole batch.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "c1",
		body: `{"id":"b1","journals":[` +
			batchJournalBody("jv-new", "2024-01-10", 100) + `,` +
			batchJournalBody("jv-existing", "2024-01-11", 100) + `]}`},
		409, "conflict", "already exists")

	// A duplicate id inside the batch is a conflict too.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "c2",
		body: `{"id":"b2","journals":[` +
			batchJournalBody("jv-dup", "2024-01-10", 100) + `,` +
			batchJournalBody("jv-dup", "2024-01-11", 100) + `]}`},
		409, "conflict", "duplicated")

	// Failures are atomic: no journal, no batch and no idempotency record was
	// written, so the same keys and journal ids can be reused.
	client.expect(request{method: http.MethodGet, path: "/journals/jv-new"},
		404, "not_found", "not found")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-dup"},
		404, "not_found", "not found")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/b1"},
		404, "not_found", "not found")

	status, raw, _ := client.send(request{method: http.MethodPost, path: "/journal-batches", key: "c1",
		body: `{"id":"b1","journals":[` + batchJournalBody("jv-new", "2024-01-10", 100) + `]}`})
	if status != http.StatusCreated {
		t.Fatalf("reusing the key of a failed batch returned %d: %s", status, raw)
	}

	// A committed batch id cannot be reused.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "c3",
		body: `{"id":"b1","journals":[` + batchJournalBody("jv-other", "2024-01-10", 100) + `]}`},
		409, "conflict", "already exists")

	// A journal dated inside a closed month rejects the whole batch with 409.
	client.created("/period-closes", `{"id":"pc-1","period":{"start":"2024-02-01","end":"2024-02-29"}}`)
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "c4",
		body: `{"id":"b3","journals":[` +
			batchJournalBody("jv-open", "2024-03-01", 100) + `,` +
			batchJournalBody("jv-closed", "2024-02-15", 100) + `]}`},
		409, "conflict", "closed period")
	client.expect(request{method: http.MethodGet, path: "/journals/jv-open"},
		404, "not_found", "not found")
	client.expect(request{method: http.MethodGet, path: "/journal-batches/b3"},
		404, "not_found", "not found")
}

func TestJournalBatchIdempotency(t *testing.T) {
	client := newBatchClient(t)

	body := `{"id":"b1","journals":[` +
		batchJournalBody("jv-a", "2024-01-10", 100) + `,` +
		batchJournalBody("jv-b", "2024-01-11", 200) + `]}`
	status, first, _ := client.send(request{method: http.MethodPost, path: "/journal-batches", key: "same-key", body: body})
	if status != http.StatusCreated {
		t.Fatalf("first batch returned %d: %s", status, first)
	}
	status, replay, _ := client.send(request{method: http.MethodPost, path: "/journal-batches", key: "same-key", body: body})
	if status != http.StatusCreated || replay != first {
		t.Fatalf("replay returned %d: %s, want the first response %s", status, replay, first)
	}

	// The replay wrote nothing: the journals and the batch still exist exactly
	// once and the batch record is unchanged.
	if got := client.ok("/journal-batches/b1"); integer(t, got, "journal_count") != 2 {
		t.Fatalf("batch changed after replay: %v", got)
	}

	// The same key on another operation conflicts.
	client.expect(request{method: http.MethodPost, path: "/journal-batches", key: "same-key",
		body: `{"id":"b2","journals":[` + batchJournalBody("jv-c", "2024-01-12", 100) + `]}`},
		409, "conflict", "another operation")
	client.expect(request{method: http.MethodPost, path: "/journals", key: "same-key",
		body: batchJournalBody("jv-d", "2024-01-12", 100)},
		409, "conflict", "another operation")
}

func TestJournalBatchSurvivesReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "ledger.db")
	client := newClientOn(t, database)
	client.created("/accounts", accountBody("1000", "Cash", "asset", "CNY"))
	client.created("/accounts", accountBody("4000", "Revenue", "revenue", "CNY"))
	client.created("/journal-batches", `{"id":"b1","journals":[`+
		batchJournalBody("jv-a", "2024-01-10", 100)+`,`+
		batchJournalBody("jv-b", "2024-01-11", 200)+`]}`)

	reopened := newClientOn(t, database)
	batch := reopened.ok("/journal-batches/b1")
	if integer(t, batch, "journal_count") != 2 {
		t.Fatalf("batch did not survive the reopen: %v", batch)
	}
	ids := batch["journal_ids"].([]any)
	if len(ids) != 2 || ids[0] != "jv-a" || ids[1] != "jv-b" {
		t.Fatalf("journal order changed across the reopen: %v", ids)
	}
	if integer(t, reopened.ok("/journals/jv-b"), "debit_functional_minor") != 200 {
		t.Fatalf("journal jv-b did not survive the reopen")
	}

	// Later activity never rewrites the committed batch record.
	status, raw, _ := reopened.send(request{
		method: http.MethodPost, path: "/journals", key: "reopened-1",
		body: batchJournalBody("jv-later", "2024-01-12", 300),
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /journals after reopen returned %d: %s", status, raw)
	}
	again := reopened.ok("/journal-batches/b1")
	if integer(t, again, "journal_count") != 2 || len(again["journal_ids"].([]any)) != 2 {
		t.Fatalf("later activity rewrote the batch: %v", again)
	}
}
