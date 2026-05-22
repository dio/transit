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
	mux.HandleFunc("GET /routes.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, store.Current())
	})
	mux.HandleFunc("PUT /routes.json", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var cfg RouteConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, "invalid route config")
			return
		}
		if err := store.Replace(cfg); err != nil {
			writeError(w, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, store.Current())
	})
	mux.HandleFunc("POST /models", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var update ModelUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			writeError(w, "invalid model update")
			return
		}
		if err := store.UpsertModel(update); err != nil {
			writeError(w, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, store.Current())
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
			"upstream": name,
			"auth":     r.Header.Get("authorization"),
			"provider": r.Header.Get("x-llm-provider"),
			"version":  r.Header.Get("x-cluster-router-version"),
			"model":    r.Header.Get("x-model"),
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

func writeError(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal json: %v", err))
	}
	return raw
}
