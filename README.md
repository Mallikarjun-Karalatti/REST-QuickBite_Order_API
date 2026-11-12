# Exercise 01 — REST: The QuickBite Order API (Answers)

## Step 0 — Design Before You Code

| User action | Method | Path | Success code | Failure codes | Notes |
|---|---|---|---|---|---|
| See the menu | `GET` | `/menu` | **200 OK** | 405 | Returns list of menu items. |
| Place an order | `POST` | `/orders` | **201 Created** | 400, 429 | Validates items/quantities; returns `Location: /orders/{id}` header. |
| View one order | `GET` | `/orders/{id}` | **200 OK** | 404 | Returns order details or 404 if missing (supports `ETag` + `304`). |
| See my orders, 10 at a time | `GET` | `/orders?limit=10&offset=0` | **200 OK** | 400 | Supports status filtering, offset/cursor pagination, and returns total count. |
| Cancel an order | `PATCH` | `/orders/{id}` | **200 OK** | 400, 404, 409 | Body: `{"status":"CANCELLED"}`. Only allowed while order is `PLACED`. |

---

## Step 3 — Think & Answer

### 1. What's the difference between 400, 404, 409, and 422? Which did you use where, and why?
* **400 Bad Request:** Syntactic or client contract violation (e.g. malformed JSON, missing required `customerId`, invalid `limit` or `status` string, customer trying to patch `status: "DELIVERED"`). Used in `POST /orders`, `GET /orders`, and `PATCH /orders/{id}`.
* **404 Not Found:** The targeted resource URI does not exist on the server. Used in `GET /orders/{id}` and `PATCH /orders/{id}` when the order ID is not found.
* **409 Conflict:** The request is syntactically valid, but cannot be processed because it conflicts with the current server state of the target resource. Used in `PATCH /orders/{id}` when attempting to cancel an order that is already in `PREPARING` or delivered.
* **422 Unprocessable Content:** The request structure/JSON is valid, but contains semantic/business domain validation errors (often used in modern REST APIs to return field-level validation errors).

---

### 2. `GET`, `PUT`, `DELETE` are idempotent; `POST` isn't. What does that mean for a client that times out?
* For idempotent methods (`GET`, `PUT`, `DELETE`), if a client request times out or disconnects before receiving a response, the client can safely retry automatically because applying the operation multiple times produces the exact same server state.
* For `POST`, a timeout leaves the client in an indeterminate state: the server may have created the order and charged the customer before the connection dropped. Retrying blindly can result in duplicate orders and double charges, which is why non-idempotent `POST` endpoints require an `Idempotency-Key` header so retries replay the original response instead of creating duplicates.

---

### 3. Why is there no `DELETE /orders/{id}`? (Hint: would Swiggy ever *delete* your order history?)
Orders represent financial transactions, operational contracts, and legal audit trails. Physical deletion would destroy accounting ledgers, tax documentation, driver payout history, restaurant settlement logs, and dispute evidence. Real-world systems use state transitions (e.g. `status = CANCELLED`) or soft deletion rather than removing records from the database.

---

### 4. Cancelling is modelled as `PATCH {"status":"CANCELLED"}`. Another option is `POST /orders/{id}/cancel`. Argue for each.
* **Argument for `PATCH /orders/{id}` (Resource-Oriented / Strict REST):**
  Treats the order as a resource and cancellation as a partial mutation of its properties (`status`). Keeps URIs consistent as nouns and avoids bloating the API surface with verb-based endpoints.
* **Argument for `POST /orders/{id}/cancel` (Action-Oriented / RPC-over-HTTP):**
  Cancellation is a complex business workflow (triggers payment refunds, sends push notifications, notifies restaurant dispatchers, and may require cancellation metadata like reasons). Modeling it as an explicit action sub-resource clarifies authorization, auditing, and payload validation.

---

### 5. Offset pagination breaks when new orders arrive while a user scrolls. What is cursor pagination and how would `GET /orders` change?
* **Why Offset Breaks:** In `limit=10&offset=10`, if new orders are inserted at the top of the table while a user navigates between pages, the window shifts down and the user sees duplicate items on page 2 that were already shown on page 1.
* **How Cursor Pagination Solves It:** Instead of skipping an arbitrary row count, the client passes an opaque pointer referencing the last seen item: `GET /orders?limit=10&cursor=ord_1010`. The server queries `WHERE id < cursor ORDER BY id DESC LIMIT 10`, ensuring stable pagination regardless of concurrent insertions or deletions. The response returns a `nextCursor` token for the subsequent page.

---

### 6. Where would API versioning go — `/v1/orders`, a header, or a query param? Pick one and defend it.
* **Chosen Approach:** URI path versioning (`/v1/orders`).
* **Defense:**
  1. **Visibility & Operability:** The version is clearly visible in gateway routing rules (Envoy, Nginx), CDN caches, access logs, and monitoring dashboards without inspecting request headers or query strings.
  2. **Client Simplicity:** Easy to test and share via standard browsers and `curl` without requiring custom HTTP headers (`Accept: application/vnd.quickbite.v1+json`).
  3. **Predictable Caching:** Query parameter versioning (`/orders?v=1`) is fragile because CDNs or caching proxies often strip or normalize query parameters.

