package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func serve(t *testing.T, body string, check func(*http.Request)) Querier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	q, err := New(srv.URL, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQuery(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantVal   float64
		wantEmpty bool
		wantErr   bool
	}{
		{"vector sums", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"2"]},{"metric":{"a":"b"},"value":[1,"3.5"]}]}}`, 5.5, false, false},
		{"empty vector", `{"status":"success","data":{"resultType":"vector","result":[]}}`, 0, true, false},
		{"scalar", `{"status":"success","data":{"resultType":"scalar","result":[1,"7"]}}`, 7, false, false},
		{"matrix rejected", `{"status":"success","data":{"resultType":"matrix","result":[]}}`, 0, false, true},
		{"NaN rejected", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`, 0, false, true},
		{"api error", `{"status":"error","errorType":"bad_data","error":"nope"}`, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotQuery, gotTime string
			q := serve(t, tt.body, func(r *http.Request) {
				_ = r.ParseForm()
				gotQuery, gotTime = r.Form.Get("query"), r.Form.Get("time")
			})
			v, empty, err := q.Query(context.Background(), "up", time.Unix(1000, 0))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v", err)
			}
			if v != tt.wantVal || empty != tt.wantEmpty {
				t.Fatalf("got %v %v", v, empty)
			}
			if gotQuery != "up" || gotTime != "1000" {
				t.Fatalf("sent query=%q time=%q", gotQuery, gotTime)
			}
		})
	}
}

func TestHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	q, _ := New(srv.URL, "", time.Second)
	if _, _, err := q.Query(context.Background(), "up", time.Now()); err == nil {
		t.Fatal("want error")
	}
	srv.Close()
	if _, _, err := q.Query(context.Background(), "up", time.Now()); err == nil {
		t.Fatal("want error when down")
	}
}

func TestBearerToken(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("s3cret\n"), 0o600)
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	q, _ := New(srv.URL, f, time.Second)
	if _, _, err := q.Query(context.Background(), "up", time.Now()); err != nil || auth != "Bearer s3cret" {
		t.Fatalf("auth=%q err=%v", auth, err)
	}
}
