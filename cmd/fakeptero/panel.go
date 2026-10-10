package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

// panel is an in-memory stand-in for the Pterodactyl Application API. It
// implements only the endpoints kuu-api calls, with the same JSON shapes,
// and logs every call so the demo shows what happened.
type panel struct {
	mu          sync.Mutex
	nodeID      int
	fqdn        string
	memoryMB    int
	diskMB      int
	nextUserID  int
	nextSrvID   int
	users       map[int]pterodactyl.UserAttributes
	servers     map[int]*server
	allocations []*pterodactyl.AllocationAttributes

	// onUserCreated runs after a user is created without a password, the
	// way the real panel emails its "Account Created" setup link.
	onUserCreated func(pterodactyl.UserAttributes)
}

type server struct {
	ID           int
	Name         string
	User         int
	Memory       int
	Disk         int
	AllocationID int
	Suspended    bool
}

// portRange is a block of allocations to seed on the node.
type portRange struct{ min, max int }

func newPanel(nodeID int, fqdn string, memoryMB, diskMB int, ranges []portRange) *panel {
	p := &panel{
		nodeID:   nodeID,
		fqdn:     fqdn,
		memoryMB: memoryMB,
		diskMB:   diskMB,
		users:    map[int]pterodactyl.UserAttributes{},
		servers:  map[int]*server{},
	}
	id := 0
	for _, r := range ranges {
		for port := r.min; port <= r.max; port++ {
			id++
			p.allocations = append(p.allocations, &pterodactyl.AllocationAttributes{ID: id, Port: port})
		}
	}
	return p
}

func (p *panel) routes() http.Handler {
	mux := http.NewServeMux()
	node := fmt.Sprintf("/api/application/nodes/%d", p.nodeID)

	mux.HandleFunc("GET /api/application/nodes", p.listNodes)
	mux.HandleFunc("GET "+node, p.getNode)
	mux.HandleFunc("GET "+node+"/allocations", p.listAllocations)
	mux.HandleFunc("GET /api/application/users", p.searchUsers)
	mux.HandleFunc("POST /api/application/users", p.createUser)
	mux.HandleFunc("DELETE /api/application/users/{id}", p.deleteUser)
	mux.HandleFunc("GET /api/application/servers", p.listServers)
	mux.HandleFunc("POST /api/application/servers", p.createServer)
	mux.HandleFunc("POST /api/application/servers/{id}/suspend", p.setSuspended(true))
	mux.HandleFunc("POST /api/application/servers/{id}/unsuspend", p.setSuspended(false))
	mux.HandleFunc("DELETE /api/application/servers/{id}", p.deleteServer)

	return requireAPIKey(logRequests(mux))
}

// requireAPIKey rejects requests without a bearer token, like the real
// panel. Any token is accepted.
func requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"errors":[{"code":"AuthenticationException"}]}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%-6s %-50s → %d", r.Method, r.URL.RequestURI(), rec.status)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	return id, err == nil
}

// --- nodes -----------------------------------------------------------------

func (p *panel) listNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"attributes": map[string]any{"id": p.nodeID, "fqdn": p.fqdn}}}})
}

func (p *panel) getNode(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	usedMem, usedDisk := 0, 0
	for _, s := range p.servers {
		usedMem += s.Memory
		usedDisk += s.Disk
	}
	writeJSON(w, http.StatusOK, map[string]any{"attributes": map[string]any{
		"id":     p.nodeID,
		"fqdn":   p.fqdn,
		"memory": p.memoryMB,
		"disk":   p.diskMB,
		"allocated_resources": map[string]int{
			"memory": usedMem,
			"disk":   usedDisk,
		},
	}})
}

func (p *panel) listAllocations(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	resp := pterodactyl.AllocationListResponse{}
	for _, a := range p.allocations {
		resp.Data = append(resp.Data, pterodactyl.AllocationData{Attributes: *a})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- users -----------------------------------------------------------------

func (p *panel) searchUsers(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	want := r.URL.Query().Get("filter[username]")
	data := []pterodactyl.UserResponse{}
	for _, u := range p.users {
		if want == "" || strings.EqualFold(u.Username, want) {
			data = append(data, pterodactyl.UserResponse{Attributes: u})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (p *panel) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" || req.Username == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "email and username are required"})
		return
	}

	p.mu.Lock()
	for _, u := range p.users {
		if strings.EqualFold(u.Username, req.Username) || strings.EqualFold(u.Email, req.Email) {
			p.mu.Unlock()
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "username or email already taken"})
			return
		}
	}
	p.nextUserID++
	user := pterodactyl.UserAttributes{ID: p.nextUserID, Username: req.Username, Email: req.Email}
	p.users[user.ID] = user
	p.mu.Unlock()

	log.Printf("panel: created user %d %q <%s>", user.ID, user.Username, user.Email)
	if req.Password == "" && p.onUserCreated != nil {
		p.onUserCreated(user)
	}
	writeJSON(w, http.StatusCreated, pterodactyl.UserResponse{Attributes: user})
}

func (p *panel) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.users[id]; !ok || !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	delete(p.users, id)
	w.WriteHeader(http.StatusNoContent)
}

// --- servers ---------------------------------------------------------------

func (p *panel) listServers(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int, 0, len(p.servers))
	for id := range p.servers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	data := []any{}
	for _, id := range ids {
		s := p.servers[id]
		data = append(data, map[string]any{"attributes": map[string]any{
			"id": s.ID, "name": s.Name, "user": s.User, "node": p.nodeID, "suspended": s.Suspended,
		}})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"pagination": map[string]int{"current_page": 1, "total_pages": 1}},
	})
}

func (p *panel) createServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		User   int    `json:"user"`
		Limits struct {
			Memory int `json:"memory"`
			Disk   int `json:"disk"`
		} `json:"limits"`
		Allocation struct {
			Default int `json:"default"`
		} `json:"allocation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid body"})
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.users[req.User]; !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unknown user"})
		return
	}
	var alloc *pterodactyl.AllocationAttributes
	for _, a := range p.allocations {
		if a.ID == req.Allocation.Default {
			alloc = a
		}
	}
	// The real panel answers 422 when the allocation was taken in the
	// meantime; kuu-api retries on a fresh pick.
	if alloc == nil || alloc.Assigned {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "allocation unavailable"})
		return
	}
	alloc.Assigned = true

	p.nextSrvID++
	s := &server{ID: p.nextSrvID, Name: req.Name, User: req.User, Memory: req.Limits.Memory, Disk: req.Limits.Disk, AllocationID: alloc.ID}
	p.servers[s.ID] = s
	log.Printf("panel: created server %d %q for user %d on %s:%d (%d MB)", s.ID, s.Name, s.User, p.fqdn, alloc.Port, s.Memory)

	writeJSON(w, http.StatusCreated, pterodactyl.ServerResponse{Attributes: pterodactyl.ServerAttributes{
		ID: s.ID, Name: s.Name, Identifier: fmt.Sprintf("demo%04d", s.ID),
	}})
}

func (p *panel) setSuspended(suspended bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r)
		p.mu.Lock()
		defer p.mu.Unlock()
		s, exists := p.servers[id]
		if !ok || !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.Suspended = suspended
		log.Printf("panel: server %d suspended=%v", id, suspended)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (p *panel) deleteServer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	p.mu.Lock()
	defer p.mu.Unlock()
	s, exists := p.servers[id]
	if !ok || !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for _, a := range p.allocations {
		if a.ID == s.AllocationID {
			a.Assigned = false
		}
	}
	delete(p.servers, id)
	log.Printf("panel: deleted server %d", id)
	w.WriteHeader(http.StatusNoContent)
}
