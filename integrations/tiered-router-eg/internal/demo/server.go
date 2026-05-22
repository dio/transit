package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

func RunControl(ctx context.Context, addr string, initialJSON []byte) error {
	store, err := NewConfigStoreJSON(initialJSON)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)
	return runHTTP(ctx, addr, mux)
}

func RegisterControlHandlers(mux *http.ServeMux, store *ConfigStore) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /l1/shards.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, store.L1())
	})
	mux.HandleFunc("POST /l1/shards", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var update ShardUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			writeError(w, http.StatusBadRequest, "invalid shard update")
			return
		}
		if err := store.UpsertShard(update); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, store.L1())
	})
	mux.HandleFunc("GET /l2/{shard}/routes.json", func(w http.ResponseWriter, r *http.Request) {
		shard := r.PathValue("shard")
		cfg, ok := store.L2(shard)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown shard")
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	})
	mux.HandleFunc("POST /l2/{shard}/models", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var update ModelUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			writeError(w, http.StatusBadRequest, "invalid model update")
			return
		}
		update.Shard = r.PathValue("shard")
		if err := store.UpsertModel(update); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		cfg, _ := store.L2(update.Shard)
		writeJSON(w, http.StatusOK, cfg)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, store.Dump())
	})
}

func RunUpstream(ctx context.Context, addr, name string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"upstream":    name,
			"l1_tag":      r.Header.Get("x-transit-tag"),
			"l1_source":   r.Header.Get("x-transit-tag-source"),
			"l1_shard":    r.Header.Get("x-transit-l1-shard"),
			"l1_target":   r.Header.Get("x-transit-l1-target"),
			"l1_version":  r.Header.Get("x-cluster-shard-router-version"),
			"model":       r.Header.Get("x-model"),
			"provider":    r.Header.Get("x-llm-provider"),
			"profile":     r.Header.Get("x-user-profile"),
			"byok_key_id": r.Header.Get("x-byok-key-id"),
			"auth":        r.Header.Get("authorization"),
			"l2_version":  r.Header.Get("x-cluster-router-version"),
		})
	})
	return runHTTP(ctx, addr, mux)
}

func runHTTP(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		errc <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errc
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal json: %v", err))
	}
	return raw
}
