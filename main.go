package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const port = ":8081"

// ---------- Domain ----------

type Status string

const (
	StatusPlaced         Status = "PLACED"
	StatusPreparing      Status = "PREPARING"
	StatusOutForDelivery Status = "OUT_FOR_DELIVERY"
	StatusDelivered      Status = "DELIVERED"
	StatusCancelled      Status = "CANCELLED"
)

type MenuItem struct {
	ItemID string `json:"itemId"`
	Name   string `json:"name"`
	Price  int    `json:"price"`
}

type OrderItem struct {
	ItemID string `json:"itemId"`
	Qty    int    `json:"qty"`
}

type Order struct {
	ID         string      `json:"id"`
	CustomerID string      `json:"customerId"`
	Items      []OrderItem `json:"items"`
	Total      int         `json:"total"`
	Status     Status      `json:"status"`
	CreatedAt  string      `json:"createdAt"`
}

var menuList = []MenuItem{
	{ItemID: "paneer-tikka", Name: "Paneer Tikka", Price: 249},
	{ItemID: "masala-dosa", Name: "Masala Dosa", Price: 129},
	{ItemID: "veg-biryani", Name: "Veg Biryani", Price: 219},
	{ItemID: "gulab-jamun", Name: "Gulab Jamun", Price: 79},
}

var menuMap = func() map[string]MenuItem {
	m := make(map[string]MenuItem, len(menuList))
	for _, item := range menuList {
		m[item.ItemID] = item
	}
	return m
}()

// ---------- In-memory Store ----------

type Store struct {
	mu              sync.RWMutex
	seq             int
	orders          map[string]Order
	orderIDs        []string          // preserves insertion order for pagination
	idempotencyKeys map[string]string // Idempotency-Key -> orderID
	customerOrders  map[string][]time.Time
}

func newStore() *Store {
	return &Store{
		seq:             1000,
		orders:          make(map[string]Order),
		idempotencyKeys: make(map[string]string),
		customerOrders:  make(map[string][]time.Time),
	}
}

// ---------- Server & Routing ----------

type Server struct {
	store           *Store
	rateLimitPerMin int
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/menu", s.handleMenu)
	mux.HandleFunc("/orders", s.handleOrders)
	mux.HandleFunc("/orders/{id}", s.handleOrderByID)
	mux.HandleFunc("/admin/orders/{id}/advance", s.handleAdminAdvance)
	return mux
}

func main() {
	srv := &Server{
		store:           newStore(),
		rateLimitPerMin: 5,
	}

	log.Printf("QuickBite Order API running on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, srv.routes()))
}

func (s *Server) handleMenu(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	writeJSON(w, r, http.StatusOK, menuList)
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createOrder(w, r)
	case http.MethodGet:
		s.listOrders(w, r)
	default:
		methodNotAllowed(w, r, "GET, POST")
	}
}

func (s *Server) handleOrderByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		s.getOrder(w, r, id)
	case http.MethodPatch:
		s.patchOrder(w, r, id)
	default:
		// Why no DELETE? (README question 3)
		methodNotAllowed(w, r, "GET, PATCH")
	}
}

// ---------- TODO Handlers (currently returning 501) ----------

type createOrderRequest struct {
	CustomerID string      `json:"customerId"`
	Items      []OrderItem `json:"items"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))

	// 1. Check if we have already processed this idempotency key (fast path)
	if idemKey != "" {
		s.store.mu.RLock()
		if existingID, found := s.store.idempotencyKeys[idemKey]; found {
			order := s.store.orders[existingID]
			s.store.mu.RUnlock()

			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, r, http.StatusOK, order)
			return
		}
		s.store.mu.RUnlock()
	}

	// 2. Parse & validate request body
	var req createOrderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "Malformed JSON: "+err.Error())
		return
	}

	customerID := strings.TrimSpace(req.CustomerID)
	if customerID == "" {
		writeError(w, r, http.StatusBadRequest, "customerId is required")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, r, http.StatusBadRequest, "items must be a non-empty list")
		return
	}

	total := 0
	for _, item := range req.Items {
		menuItem, ok := menuMap[item.ItemID]
		if !ok {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown itemId: %q", item.ItemID))
			return
		}
		if item.Qty < 1 || item.Qty > 20 {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("qty for %q must be between 1 and 20", item.ItemID))
			return
		}
		total += menuItem.Price * item.Qty
	}

	// 3. Create and store order under write lock (re-checking idempotency key & rate limit)
	s.store.mu.Lock()
	if idemKey != "" {
		if existingID, found := s.store.idempotencyKeys[idemKey]; found {
			order := s.store.orders[existingID]
			s.store.mu.Unlock()

			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, r, http.StatusOK, order)
			return
		}
	}

	// Rate limiting: max N orders per minute per customer
	if s.rateLimitPerMin > 0 {
		now := time.Now()
		cutoff := now.Add(-1 * time.Minute)
		var recent []time.Time
		for _, t := range s.store.customerOrders[customerID] {
			if t.After(cutoff) {
				recent = append(recent, t)
			}
		}

		if len(recent) >= s.rateLimitPerMin {
			oldest := recent[0]
			retryAfter := int(oldest.Add(1*time.Minute).Sub(now).Seconds()) + 1
			if retryAfter < 1 {
				retryAfter = 1
			}
			s.store.mu.Unlock()

			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, r, http.StatusTooManyRequests, fmt.Sprintf("rate limit exceeded: max %d orders per minute (retry after %ds)", s.rateLimitPerMin, retryAfter))
			return
		}

		recent = append(recent, now)
		s.store.customerOrders[customerID] = recent
	}

	s.store.seq++
	order := Order{
		ID:         fmt.Sprintf("ord_%d", s.store.seq),
		CustomerID: customerID,
		Items:      req.Items,
		Total:      total,
		Status:     StatusPlaced,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.store.orders[order.ID] = order
	s.store.orderIDs = append(s.store.orderIDs, order.ID)

	if idemKey != "" {
		s.store.idempotencyKeys[idemKey] = order.ID
	}
	s.store.mu.Unlock()

	w.Header().Set("Location", "/orders/"+order.ID)
	writeJSON(w, r, http.StatusCreated, order)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request, id string) {
	s.store.mu.RLock()
	order, ok := s.store.orders[id]
	s.store.mu.RUnlock()

	if !ok {
		writeError(w, r, http.StatusNotFound, "Order not found")
		return
	}

	raw, err := json.Marshal(order)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "failed to serialize order")
		return
	}

	// Compute ETag as SHA-256 hash of the order's JSON payload
	hash := sha256.Sum256(raw)
	etag := fmt.Sprintf(`"%x"`, hash[:8])
	w.Header().Set("ETag", etag)

	// If client provided matching ETag, return 304 Not Modified with empty body
	if r.Header.Get("If-None-Match") == etag {
		fmt.Printf("%-6s %-40s -> %d (Not Modified)\n", r.Method, r.URL.RequestURI(), http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	fmt.Printf("%-6s %-40s -> %d\n", r.Method, r.URL.RequestURI(), http.StatusOK)
}

type listOrdersResponse struct {
	Items      []Order `json:"items"`
	Total      int     `json:"total"`
	Limit      int     `json:"limit"`
	Offset     int     `json:"offset"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// 1. Validate status filter
	statusStr := q.Get("status")
	if statusStr != "" {
		switch Status(statusStr) {
		case StatusPlaced, StatusPreparing, StatusOutForDelivery, StatusDelivered, StatusCancelled:
			// valid status
		default:
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid status: %q", statusStr))
			return
		}
	}

	// 2. Validate limit (default 10, max 50)
	limit := 10
	if lStr := q.Get("limit"); lStr != "" {
		val, err := strconv.Atoi(lStr)
		if err != nil || val < 1 || val > 50 {
			writeError(w, r, http.StatusBadRequest, "limit must be an integer between 1 and 50")
			return
		}
		limit = val
	}

	// 3. Filter orders matching status
	s.store.mu.RLock()
	var matched []Order
	for _, id := range s.store.orderIDs {
		order := s.store.orders[id]
		if statusStr != "" && string(order.Status) != statusStr {
			continue
		}
		matched = append(matched, order)
	}
	s.store.mu.RUnlock()

	total := len(matched)
	start := 0
	cursor := q.Get("cursor")

	// 4. Cursor vs. Offset pagination
	if cursor != "" {
		cursorIdx := -1
		for i, o := range matched {
			if o.ID == cursor {
				cursorIdx = i
				break
			}
		}
		if cursorIdx == -1 {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid cursor: order %q not found", cursor))
			return
		}
		start = cursorIdx + 1
	} else if oStr := q.Get("offset"); oStr != "" {
		val, err := strconv.Atoi(oStr)
		if err != nil || val < 0 {
			writeError(w, r, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		start = val
	}

	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	page := matched[start:end]
	if page == nil {
		page = []Order{}
	}

	nextCursor := ""
	if end < total && len(page) > 0 {
		nextCursor = page[len(page)-1].ID
	}

	writeJSON(w, r, http.StatusOK, listOrdersResponse{
		Items:      page,
		Total:      total,
		Limit:      limit,
		Offset:     start,
		NextCursor: nextCursor,
	})
}

type patchOrderRequest struct {
	Status string `json:"status"`
}

func (s *Server) patchOrder(w http.ResponseWriter, r *http.Request, id string) {
	var req patchOrderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "Malformed JSON: "+err.Error())
		return
	}

	// Customers may only cancel; setting anything else (e.g. DELIVERED) is forbidden
	if req.Status != string(StatusCancelled) {
		writeError(w, r, http.StatusBadRequest, "status must be CANCELLED")
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	order, ok := s.store.orders[id]
	if !ok {
		writeError(w, r, http.StatusNotFound, "Order not found")
		return
	}

	// Business rule: orders can only be cancelled while in PLACED state
	if order.Status != StatusPlaced {
		writeError(w, r, http.StatusConflict, fmt.Sprintf("cannot cancel an order that is %s", order.Status))
		return
	}

	order.Status = StatusCancelled
	s.store.orders[id] = order

	writeJSON(w, r, http.StatusOK, order)
}

func readJSON(r *http.Request, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return fmt.Errorf("request body is required")
	}
	return json.Unmarshal(raw, dst)
}

// ---------- Given Admin Endpoint ----------

func (s *Server) handleAdminAdvance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	id := r.PathValue("id")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	o, ok := s.store.orders[id]
	if !ok {
		writeError(w, r, http.StatusNotFound, "Order not found")
		return
	}

	switch o.Status {
	case StatusPlaced:
		o.Status = StatusPreparing
	case StatusPreparing:
		o.Status = StatusOutForDelivery
	case StatusOutForDelivery, StatusDelivered:
		o.Status = StatusDelivered
	case StatusCancelled:
		// stays CANCELLED
	}

	s.store.orders[id] = o
	writeJSON(w, r, http.StatusOK, o)
}

// ---------- HTTP Helpers ----------

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	fmt.Printf("%-6s %-40s -> %d\n", r.Method, r.URL.RequestURI(), status)
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	writeJSON(w, r, status, map[string]string{"error": msg})
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, r, http.StatusMethodNotAllowed, r.Method+" not allowed here. Allowed: "+allow)
}
