package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
}

func statusFor(k errs.Kind) int {
	switch k {
	case errs.KindInvalid:
		return http.StatusBadRequest
	case errs.KindNotFound:
		return http.StatusNotFound
	case errs.KindConflict:
		return http.StatusConflict
	case errs.KindUnauthorized:
		return http.StatusUnauthorized
	case errs.KindForbidden:
		return http.StatusForbidden
	case errs.KindUnavailable:
		return http.StatusServiceUnavailable
	case errs.KindTimeout:
		return http.StatusGatewayTimeout
	case errs.KindPrecondition:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// writeError renders err. Unclassified errors never leak internals.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	body := errorBody{Error: errorDetail{RequestID: requestIDFrom(r.Context())}}
	status := http.StatusInternalServerError
	if e, ok := errs.As(err); ok {
		status = statusFor(e.Kind)
		body.Error.Code, body.Error.Message, body.Error.Details = e.Code, e.Message, e.Details
		if e.Kind == errs.KindInternal {
			body.Error.Message = "internal error"
		}
	} else {
		body.Error.Code, body.Error.Message = "internal_error", "internal error"
	}
	writeJSON(w, status, body)
}

func notFoundRoute() error {
	return errs.NotFound("route_not_found", "no such endpoint")
}

// decode reads a JSON body into v, rejecting unknown fields and trailing
// data. An empty body leaves v unchanged when allowEmpty is set.
func decode(r *http.Request, v any, allowEmpty bool) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return errs.New(errs.KindInvalid, "unsupported_media_type", "Content-Type must be application/json")
		}
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF) && allowEmpty:
			return nil
		case errors.Is(err, io.EOF):
			return errs.Invalid("empty_body", "request body is required")
		case errors.As(err, &mbe):
			return errs.Invalid("body_too_large", "request body exceeds %d bytes", mbe.Limit)
		default:
			return errs.Invalid("invalid_json", "invalid JSON body: %s", sanitizeJSONErr(err))
		}
	}
	if dec.More() {
		return errs.Invalid("invalid_json", "request body must contain a single JSON object")
	}
	return nil
}

func sanitizeJSONErr(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("syntax error at offset %d", se.Offset)
	case errors.As(err, &te):
		return fmt.Sprintf("field %q has the wrong type", te.Field)
	default:
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return msg
	}
}

func boolQuery(r *http.Request, name string, def bool) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, errs.Invalid("invalid_query", "query parameter %s must be true or false", name)
	}
	return b, nil
}
