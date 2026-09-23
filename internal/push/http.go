// SPDX-License-Identifier: GPL-3.0-or-later

package push

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Register adds the key and subscription endpoints to mux. A subscription
// posted with ?confirm=1 gets a confirmation notification, so the browser
// shows at once that push reaches it.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /push/key", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"key": s.publicKey})
	})
	mux.HandleFunc("POST /push/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		var sub Subscription
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&sub); err != nil {
			http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := sub.validate(); err != nil {
			http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Subscribe(sub); err != nil {
			s.logger.Error("push subscribe failed", "err", err)
			http.Error(w, "could not store the subscription", http.StatusInternalServerError)
			return
		}
		if r.URL.Query().Get("confirm") != "" {
			go s.SendTo(context.Background(), sub, Message{
				Title: "cc-proxy notifications are on",
				Body:  "Permission prompts and waiting sessions will show up here.",
				Tag:   "welcome",
				TTL:   time.Hour,
			})
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /push/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		var sub Subscription
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&sub); err != nil {
			http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Unsubscribe(sub.Endpoint); err != nil {
			s.logger.Error("push unsubscribe failed", "err", err)
			http.Error(w, "could not remove the subscription", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
