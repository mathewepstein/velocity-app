package devs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestChunkStrings(t *testing.T) {
	cases := []struct {
		in   []string
		size int
		want [][]string
	}{
		{nil, 50, [][]string{}},
		{[]string{"a"}, 50, [][]string{{"a"}}},
		{[]string{"a", "b", "c", "d", "e"}, 2, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}},
		{[]string{"a", "b"}, 0, [][]string{{"a", "b"}}}, // size<=0 returns single chunk
	}
	for i, tc := range cases {
		got := chunkStrings(tc.in, tc.size)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("case %d: chunkStrings(%v, %d) = %v, want %v", i, tc.in, tc.size, got, tc.want)
		}
	}
}

func TestUniqueNonEmpty(t *testing.T) {
	got := uniqueNonEmpty([]string{"a", "", "b", "a", "c", ""})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uniqueNonEmpty = %v, want %v", got, want)
	}
}

func TestResolveJiraNamesPaginatesAndMerges(t *testing.T) {
	// Mock Jira bulk endpoint: first page returns 2 users + a nextPage link,
	// second page returns the third user with isLast=true. Validates we
	// follow pagination AND that batched accountId params reach the server.
	var seenIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/user/bulk", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		seenIDs = append(seenIDs, r.Form["accountId"]...)
		page := r.URL.Query().Get("page")
		if page == "" {
			next := "http://" + r.Host + r.URL.Path + "?" + url.Values{"page": []string{"2"}}.Encode()
			_, _ = w.Write([]byte(`{"values":[{"accountId":"a1","displayName":"Alice"},{"accountId":"a2","displayName":"Bob"}],"isLast":false,"nextPage":"` + next + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"values":[{"accountId":"a3","displayName":"Carol"}],"isLast":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config.JiraConfig{BaseURL: srv.URL, Email: "user@example.com"}
	got, err := ResolveJiraNames(context.Background(), cfg, "tok", []string{"a1", "a2", "a3"})
	if err != nil {
		t.Fatalf("ResolveJiraNames: %v", err)
	}
	want := map[string]string{"a1": "Alice", "a2": "Bob", "a3": "Carol"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveJiraNames = %v, want %v", got, want)
	}

	// First page must have sent every requested accountId — pagination is
	// only over the response, not the request batch.
	if want := []string{"a1", "a2", "a3"}; !reflect.DeepEqual(seenIDs[:3], want) {
		t.Errorf("first page sent %v, want %v", seenIDs[:3], want)
	}
}

func TestResolveJiraNamesEmptyInput(t *testing.T) {
	cfg := config.JiraConfig{BaseURL: "http://example.invalid", Email: "user@example.com"}
	got, err := ResolveJiraNames(context.Background(), cfg, "tok", nil)
	if err != nil {
		t.Fatalf("ResolveJiraNames: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty input should return empty map, got %v", got)
	}
}

func TestResolveJiraNamesPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	cfg := config.JiraConfig{BaseURL: srv.URL, Email: "user@example.com"}
	_, err := ResolveJiraNames(context.Background(), cfg, "tok", []string{"a1"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("want 403 error, got %v", err)
	}
}
