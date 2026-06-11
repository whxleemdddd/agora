package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
)

const APIPort = 7981

// Server Agora HTTP API + Web Dashboard
type Server struct {
	ag   *Agora
	mux  *http.ServeMux
	srv  *http.Server
}

func NewAPIServer(ag *Agora) *Server {
	s := &Server{ag: ag, mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/peers", s.handlePeers)
	s.mux.HandleFunc("/api/self", s.handleSelf)
	// Web Dashboard
	s.mux.Handle("/", http.FileServer(http.FS(webFS)))
	return s
}

func (s *Server) Start(ctx context.Context) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", APIPort))
	if err != nil {
		log.Printf("[api] listen error: %v", err)
		return
	}

	s.srv = &http.Server{
		Handler: s.mux,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		if err := s.srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[api] serve error: %v", err)
		}
	}()
	log.Printf("[api] dashboard: http://127.0.0.1:%d", APIPort)
}

func (s *Server) Stop() {
	if s.srv != nil {
		s.srv.Close()
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agent_id":   s.ag.Self().AgentID,
		"name":       s.ag.Self().Name,
		"status":     s.ag.Self().Status,
		"peers":      len(s.ag.Peers()),
	})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.Peers())
}

func (s *Server) handleSelf(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ag.Self())
}
