package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() (*Server, http.Handler) {
	srv := &Server{store: newStore(), rateLimitPerMin: 5}
	return srv, srv.routes()
}

func doRequest(t *testing.T, h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("failed to decode JSON %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestGetMenu(t *testing.T) {
	_, h := newTestServer()

	rec := doRequest(t, h, http.MethodGet, "/menu", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	items := decodeJSON[[]MenuItem](t, rec)
	if len(items) != 4 {
		t.Fatalf("expected 4 menu items, got %d", len(items))
	}
}

func TestDeleteOrderMethodNotAllowed(t *testing.T) {
	_, h := newTestServer()

	rec := doRequest(t, h, http.MethodDelete, "/orders/ord_1001", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Fatal("expected Allow header on 405 response")
	}
}

func TestCreateOrder(t *testing.T) {
	_, h := newTestServer()

	body := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":2},{"itemId":"gulab-jamun","qty":1}]}`
	rec := doRequest(t, h, http.MethodPost, "/orders", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	order := decodeJSON[Order](t, rec)
	if !strings.HasPrefix(order.ID, "ord_") {
		t.Fatalf("expected order id starting with ord_, got %q", order.ID)
	}
	if order.Status != StatusPlaced {
		t.Fatalf("expected status PLACED, got %q", order.Status)
	}
	if order.Total != 337 {
		t.Fatalf("expected total 337, got %d", order.Total)
	}
	if loc := rec.Header().Get("Location"); loc != "/orders/"+order.ID {
		t.Fatalf("expected Location %q, got %q", "/orders/"+order.ID, loc)
	}
}

func TestCreateOrderValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing customerId", `{"items":[{"itemId":"masala-dosa","qty":1}]}`},
		{"blank customerId", `{"customerId":"   ","items":[{"itemId":"masala-dosa","qty":1}]}`},
		{"empty items", `{"customerId":"c1","items":[]}`},
		{"unknown itemId", `{"customerId":"c1","items":[{"itemId":"pizza","qty":1}]}`},
		{"qty 0", `{"customerId":"c1","items":[{"itemId":"masala-dosa","qty":0}]}`},
		{"qty > 20", `{"customerId":"c1","items":[{"itemId":"masala-dosa","qty":21}]}`},
		{"malformed JSON", `{oops`},
		{"empty body", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer()
			rec := doRequest(t, h, http.MethodPost, "/orders", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGetOrder(t *testing.T) {
	_, h := newTestServer()

	// 1. Create an order first
	createBody := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":1}]}`
	createRec := doRequest(t, h, http.MethodPost, "/orders", createBody)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 on create, got %d", createRec.Code)
	}
	created := decodeJSON[Order](t, createRec)

	// 2. Fetch it by ID -> 200
	getRec := doRequest(t, h, http.MethodGet, "/orders/"+created.ID, "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	fetched := decodeJSON[Order](t, getRec)
	if fetched.ID != created.ID || fetched.CustomerID != "cust_7" {
		t.Fatalf("fetched order mismatch: %+v vs %+v", fetched, created)
	}

	// 3. Fetch missing order -> 404
	missingRec := doRequest(t, h, http.MethodGet, "/orders/ord_999999", "")
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing order, got %d: %s", missingRec.Code, missingRec.Body.String())
	}
}

func TestListOrdersPagination(t *testing.T) {
	_, h := newTestServer()

	// Seed 4 orders
	body := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":1}]}`
	for i := 0; i < 4; i++ {
		rec := doRequest(t, h, http.MethodPost, "/orders", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("failed to seed order %d", i)
		}
	}

	// Page size 2
	rec := doRequest(t, h, http.MethodGet, "/orders?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	res := decodeJSON[listOrdersResponse](t, rec)
	if len(res.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res.Items))
	}
	if res.Total != 4 {
		t.Fatalf("expected total 4, got %d", res.Total)
	}
	if res.Limit != 2 {
		t.Fatalf("expected limit 2, got %d", res.Limit)
	}

	// Offset shifting
	rec0 := doRequest(t, h, http.MethodGet, "/orders?limit=1&offset=0", "")
	rec1 := doRequest(t, h, http.MethodGet, "/orders?limit=1&offset=1", "")
	res0 := decodeJSON[listOrdersResponse](t, rec0)
	res1 := decodeJSON[listOrdersResponse](t, rec1)

	if res0.Items[0].ID == res1.Items[0].ID {
		t.Fatalf("offset=1 should return a different item than offset=0, both got %q", res0.Items[0].ID)
	}
}

func TestListOrdersValidation(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"invalid status", "/orders?status=BOGUS"},
		{"non-numeric limit", "/orders?limit=abc"},
		{"limit exceeds 50", "/orders?limit=500"},
		{"negative limit", "/orders?limit=-1"},
		{"negative offset", "/orders?offset=-1"},
		{"non-numeric offset", "/orders?offset=xyz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer()
			rec := doRequest(t, h, http.MethodGet, tc.target, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s, got %d: %s", tc.target, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPatchOrder(t *testing.T) {
	_, h := newTestServer()

	// 1. Create order
	createBody := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":1}]}`
	rec := doRequest(t, h, http.MethodPost, "/orders", createBody)
	order := decodeJSON[Order](t, rec)

	// 2. Successful cancellation of PLACED order -> 200
	patchRec := doRequest(t, h, http.MethodPatch, "/orders/"+order.ID, `{"status":"CANCELLED"}`)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on cancel, got %d: %s", patchRec.Code, patchRec.Body.String())
	}
	cancelled := decodeJSON[Order](t, patchRec)
	if cancelled.Status != StatusCancelled {
		t.Fatalf("expected status CANCELLED, got %q", cancelled.Status)
	}

	// 3. Status filter reflects cancellation
	listRec := doRequest(t, h, http.MethodGet, "/orders?status=CANCELLED", "")
	listRes := decodeJSON[listOrdersResponse](t, listRec)
	if len(listRes.Items) != 1 || listRes.Items[0].ID != order.ID {
		t.Fatalf("expected 1 cancelled order in filter, got %d", len(listRes.Items))
	}

	// 4. Client cannot mark as DELIVERED -> 400
	rec2 := doRequest(t, h, http.MethodPost, "/orders", createBody)
	order2 := decodeJSON[Order](t, rec2)
	badStatusRec := doRequest(t, h, http.MethodPatch, "/orders/"+order2.ID, `{"status":"DELIVERED"}`)
	if badStatusRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when setting DELIVERED, got %d: %s", badStatusRec.Code, badStatusRec.Body.String())
	}

	// 5. Admin advances order to PREPARING, then client tries to cancel -> 409 Conflict
	advanceRec := doRequest(t, h, http.MethodPost, "/admin/orders/"+order2.ID+"/advance", "")
	if advanceRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from admin advance, got %d", advanceRec.Code)
	}
	conflictRec := doRequest(t, h, http.MethodPatch, "/orders/"+order2.ID, `{"status":"CANCELLED"}`)
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when cancelling PREPARING order, got %d: %s", conflictRec.Code, conflictRec.Body.String())
	}

	// 6. Missing order -> 404
	missingRec := doRequest(t, h, http.MethodPatch, "/orders/ord_999999", `{"status":"CANCELLED"}`)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing order, got %d: %s", missingRec.Code, missingRec.Body.String())
	}
}

func TestIdempotency(t *testing.T) {
	_, h := newTestServer()

	body := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":2}]}`
	key := "key-12345"

	// 1. First call -> 201 Created
	first := doRequest(t, h, http.MethodPost, "/orders", body, "Idempotency-Key", key)
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	order1 := decodeJSON[Order](t, first)

	// 2. Second call with SAME key -> 200 OK + Idempotent-Replayed: true
	second := doRequest(t, h, http.MethodPost, "/orders", body, "Idempotency-Key", key)
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 on retry, got %d: %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("expected Idempotent-Replayed: true header, got %q", second.Header().Get("Idempotent-Replayed"))
	}
	order2 := decodeJSON[Order](t, second)
	if order1.ID != order2.ID {
		t.Fatalf("retry created duplicate order: %q vs %q", order1.ID, order2.ID)
	}

	// 3. Different key -> creates a distinct new order (201)
	diffKey := doRequest(t, h, http.MethodPost, "/orders", body, "Idempotency-Key", "key-different")
	if diffKey.Code != http.StatusCreated {
		t.Fatalf("expected 201 with different key, got %d", diffKey.Code)
	}
	order3 := decodeJSON[Order](t, diffKey)
	if order3.ID == order1.ID {
		t.Fatalf("different key should not return same order id")
	}
}

func TestGetOrderETagAndNotModified(t *testing.T) {
	_, h := newTestServer()

	// 1. Create order
	createBody := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":1}]}`
	rec := doRequest(t, h, http.MethodPost, "/orders", createBody)
	order := decodeJSON[Order](t, rec)

	// 2. Initial GET returns 200 + ETag header
	getRec1 := doRequest(t, h, http.MethodGet, "/orders/"+order.ID, "")
	if getRec1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", getRec1.Code)
	}
	etag := getRec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header on GET response")
	}

	// 3. GET with If-None-Match matching ETag returns 304 Not Modified with empty body
	getRec2 := doRequest(t, h, http.MethodGet, "/orders/"+order.ID, "", "If-None-Match", etag)
	if getRec2.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", getRec2.Code)
	}
	if getRec2.Body.Len() != 0 {
		t.Fatalf("expected empty body on 304, got %q", getRec2.Body.String())
	}

	// 4. Update the order (cancel it)
	doRequest(t, h, http.MethodPatch, "/orders/"+order.ID, `{"status":"CANCELLED"}`)

	// 5. GET with the OLD ETag now returns 200 OK with the updated data and a new ETag
	getRec3 := doRequest(t, h, http.MethodGet, "/orders/"+order.ID, "", "If-None-Match", etag)
	if getRec3.Code != http.StatusOK {
		t.Fatalf("expected 200 after update, got %d", getRec3.Code)
	}
	newEtag := getRec3.Header().Get("ETag")
	if newEtag == etag {
		t.Fatalf("expected new ETag after order was cancelled, but got same: %q", newEtag)
	}
	updatedOrder := decodeJSON[Order](t, getRec3)
	if updatedOrder.Status != StatusCancelled {
		t.Fatalf("expected status CANCELLED, got %q", updatedOrder.Status)
	}
}

func TestCursorPagination(t *testing.T) {
	_, h := newTestServer()

	// 1. Seed 5 orders
	body := `{"customerId":"cust_7","items":[{"itemId":"masala-dosa","qty":1}]}`
	for i := 0; i < 5; i++ {
		doRequest(t, h, http.MethodPost, "/orders", body)
	}

	// 2. Fetch page 1 with limit=2
	rec1 := doRequest(t, h, http.MethodGet, "/orders?limit=2", "")
	res1 := decodeJSON[listOrdersResponse](t, rec1)
	if len(res1.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res1.Items))
	}
	if res1.NextCursor == "" {
		t.Fatal("expected non-empty nextCursor on page 1")
	}

	// 3. Fetch page 2 using cursor
	rec2 := doRequest(t, h, http.MethodGet, "/orders?limit=2&cursor="+res1.NextCursor, "")
	res2 := decodeJSON[listOrdersResponse](t, rec2)
	if len(res2.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res2.Items))
	}
	// Verify no items overlap
	if res2.Items[0].ID == res1.Items[0].ID || res2.Items[0].ID == res1.Items[1].ID {
		t.Fatalf("page 2 overlaps with page 1: %v vs %v", res2.Items[0].ID, res1.Items)
	}

	// 4. Fetch page 3 using next cursor (should have last 1 item and no nextCursor)
	rec3 := doRequest(t, h, http.MethodGet, "/orders?limit=2&cursor="+res2.NextCursor, "")
	res3 := decodeJSON[listOrdersResponse](t, rec3)
	if len(res3.Items) != 1 {
		t.Fatalf("expected 1 item on final page, got %d", len(res3.Items))
	}
	if res3.NextCursor != "" {
		t.Fatalf("expected empty nextCursor on final page, got %q", res3.NextCursor)
	}

	// 5. Invalid cursor -> 400
	badCursorRec := doRequest(t, h, http.MethodGet, "/orders?cursor=ord_invalid", "")
	if badCursorRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid cursor, got %d", badCursorRec.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	_, h := newTestServer()

	body := `{"customerId":"cust_spammer","items":[{"itemId":"masala-dosa","qty":1}]}`

	// 1. First 5 orders succeed (201 Created)
	for i := 1; i <= 5; i++ {
		rec := doRequest(t, h, http.MethodPost, "/orders", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected order %d to succeed with 201, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// 2. The 6th order within 1 minute gets 429 Too Many Requests
	rec6 := doRequest(t, h, http.MethodPost, "/orders", body)
	if rec6.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on 6th order, got %d: %s", rec6.Code, rec6.Body.String())
	}
	retryAfter := rec6.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header on 429 response")
	}

	// 3. Another customer should NOT be rate limited
	otherCustBody := `{"customerId":"cust_other","items":[{"itemId":"masala-dosa","qty":1}]}`
	recOther := doRequest(t, h, http.MethodPost, "/orders", otherCustBody)
	if recOther.Code != http.StatusCreated {
		t.Fatalf("expected other customer to succeed, got %d: %s", recOther.Code, recOther.Body.String())
	}
}
