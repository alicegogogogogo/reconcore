package reconcore

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const maxBodyBytes = 1 << 20

// Server adapts a Service onto net/http. It owns routing, request decoding and
// the documented error shape; every rule lives in the service layer.
type Server struct {
	service *Service
}

// NewServer builds the HTTP handler for one service.
func NewServer(service *Service) *Server {
	return &Server{service: service}
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	status, response, err := s.dispatch(request, splitPath(request.URL.Path))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, status, response)
}

func (s *Server) dispatch(request *http.Request, parts []string) (int, any, error) {
	method := request.Method
	switch {
	case method == http.MethodGet && len(parts) == 1 && parts[0] == "health":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, map[string]any{"status": "ok"}, nil

	case method == http.MethodPost && len(parts) == 1 && parts[0] == "accounts":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreateAccount(body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err

	case method == http.MethodGet && len(parts) == 1 && parts[0] == "accounts":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.ListAccounts()
		return http.StatusOK, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "accounts":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetAccount(parts[1])
		return http.StatusOK, response, err

	case method == http.MethodGet && len(parts) == 3 && parts[0] == "accounts" && parts[2] == "balance":
		if err := requireQuery(request, "as_of"); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetBalance(parts[1], request.URL.Query().Get("as_of"))
		return http.StatusOK, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "reports" && parts[1] == "trial-balance":
		if err := requireSingleQuery(request, "as_of"); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetTrialBalance(request.URL.Query().Get("as_of"))
		return http.StatusOK, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "reports" && parts[1] == "financial-statements":
		if err := requireQueries(request, "as_of", "period_start", "period_end"); err != nil {
			return 0, nil, err
		}
		query := request.URL.Query()
		response, err := s.service.GetFinancialStatements(
			query.Get("as_of"), query.Get("period_start"), query.Get("period_end"),
		)
		return http.StatusOK, response, err

	case method == http.MethodPost && len(parts) == 1 && parts[0] == "journals":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreateJournal(body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "journals":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetJournal(parts[1])
		return http.StatusOK, response, err

	case method == http.MethodPost && len(parts) == 1 && parts[0] == "rates":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreateRate(body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err

	case method == http.MethodGet && len(parts) == 1 && parts[0] == "rates":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.ListRates()
		return http.StatusOK, response, err

	case method == http.MethodPost && len(parts) == 1 && parts[0] == "statements":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreateStatement(body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "statements":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetStatement(parts[1])
		return http.StatusOK, response, err

	case method == http.MethodPost && len(parts) == 1 && parts[0] == "reconciliations":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreateReconciliation(body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "reconciliations":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetReconciliation(parts[1])
		return http.StatusOK, response, err

	case method == http.MethodPost && len(parts) == 1 && parts[0] == "period-closes":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreatePeriodClose(body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err

	case method == http.MethodGet && len(parts) == 2 && parts[0] == "period-closes":
		if err := requireQuery(request); err != nil {
			return 0, nil, err
		}
		response, err := s.service.GetPeriodClose(parts[1])
		return http.StatusOK, response, err

	case method == http.MethodPost && len(parts) == 3 && parts[0] == "period-closes" && parts[2] == "adjustments":
		body, err := readJSONBody(request)
		if err != nil {
			return 0, nil, err
		}
		response, err := s.service.CreateAdjustment(parts[1], body, request.Header.Get("Idempotency-Key"))
		return http.StatusCreated, response, err
	}
	return 0, nil, NotFoundError("route was not found")
}

func readJSONBody(request *http.Request) ([]byte, error) {
	contentType := request.Header.Get("Content-Type")
	mediaType, _, _ := strings.Cut(contentType, ";")
	if strings.TrimSpace(strings.ToLower(mediaType)) != "application/json" {
		return nil, ValidationError("Content-Type must be application/json")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
	if err != nil {
		return nil, ValidationError("request body could not be read")
	}
	if len(body) > maxBodyBytes {
		return nil, ValidationError("request body must be at most %d bytes", maxBodyBytes)
	}
	return body, nil
}

func splitPath(path string) []string {
	parts := []string{}
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// requireQuery rejects every query parameter that the route does not document.
func requireQuery(request *http.Request, allowed ...string) error {
	names := make([]string, 0, len(request.URL.Query()))
	for name := range request.URL.Query() {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		known := false
		for _, candidate := range allowed {
			if name == candidate {
				known = true
				break
			}
		}
		if !known {
			return ValidationError("unknown query parameter %s", name)
		}
	}
	return nil
}

// requireSingleQuery rejects unknown query parameters and repeated values for
// the single documented parameter, so its value can be trusted as a scalar.
func requireSingleQuery(request *http.Request, name string) error {
	query := request.URL.Query()
	if len(query[name]) > 1 {
		return ValidationError("query parameter %s must appear exactly once", name)
	}
	for candidate := range query {
		if candidate != name {
			return ValidationError("unknown query parameter %s", candidate)
		}
	}
	return nil
}

// requireQueries is requireSingleQuery for a fixed list of scalar parameters:
// every documented name must appear at most once and nothing else is allowed.
// Cardinality is checked in documented order so the first repeated parameter
// is the one named in the error.
func requireQueries(request *http.Request, names ...string) error {
	query := request.URL.Query()
	for _, name := range names {
		if len(query[name]) > 1 {
			return ValidationError("query parameter %s must appear exactly once", name)
		}
	}
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
	}
	unknown := make([]string, 0, len(query))
	for candidate := range query {
		if !known[candidate] {
			unknown = append(unknown, candidate)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return ValidationError("unknown query parameter %s", unknown[0])
	}
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	apiError := &Error{Code: "internal_error", Status: 500, Message: "internal server error"}
	var typed *Error
	if errors.As(err, &typed) {
		apiError = typed
	}
	writeJSON(writer, apiError.Status, map[string]any{
		"error": map[string]any{"code": apiError.Code, "message": apiError.Message},
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		encoded = []byte(`{"error":{"code":"internal_error","message":"internal server error"}}`)
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	writer.WriteHeader(status)
	writer.Write(encoded)
}
