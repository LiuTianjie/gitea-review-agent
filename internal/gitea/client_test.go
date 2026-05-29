package gitea

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

func testPR() model.PRRef { return model.PRRef{Owner: "acme", Repo: "widget", Number: 42} }

func TestGetDiff_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token secret-tok" {
			t.Errorf("auth header = %q, want %q", got, "token secret-tok")
		}
		if r.URL.Path != "/api/v1/repos/acme/widget/pulls/42.diff" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		io.WriteString(w, sampleDiff)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-tok", srv.Client())
	dm, err := c.GetDiff(context.Background(), testPR())
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if _, ok := dm.Files["foo.go"]; !ok {
		t.Errorf("expected foo.go in parsed diff, got %v", keys(dm.Files))
	}
}

func TestPostReview_HTTP(t *testing.T) {
	var captured reviewRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/repos/acme/widget/pulls/42/reviews" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		if got := r.Header.Get("Authorization"); got != "token T" {
			t.Errorf("auth header = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode body: %v (raw %s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":777,"state":"REQUEST_CHANGES"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "T", srv.Client())
	comments := []model.ReviewComment{
		{Path: "foo.go", Body: "issue here", NewPosition: 13, OldPosition: 0},
		{Path: "bar.go", Body: "removed bug", NewPosition: 0, OldPosition: 5},
	}
	id, err := c.PostReview(context.Background(), testPR(), "deadbeef", model.ReviewEventRequestChanges, "please fix", comments)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if id != 777 {
		t.Errorf("reviewID = %d, want 777", id)
	}

	if captured.Event != "REQUEST_CHANGES" {
		t.Errorf("event = %q, want REQUEST_CHANGES", captured.Event)
	}
	if captured.CommitID != "deadbeef" {
		t.Errorf("commit_id = %q, want deadbeef", captured.CommitID)
	}
	if captured.Body != "please fix" {
		t.Errorf("body = %q", captured.Body)
	}
	if len(captured.Comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(captured.Comments))
	}
	if captured.Comments[0].Path != "foo.go" || captured.Comments[0].NewPosition != 13 || captured.Comments[0].OldPosition != 0 {
		t.Errorf("comment[0] = %+v", captured.Comments[0])
	}
	if captured.Comments[1].Path != "bar.go" || captured.Comments[1].NewPosition != 0 || captured.Comments[1].OldPosition != 5 {
		t.Errorf("comment[1] = %+v", captured.Comments[1])
	}

	// Verify JSON tags serialize to the expected snake_case keys.
	raw, _ := json.Marshal(captured.Comments[0])
	for _, key := range []string{`"path"`, `"body"`, `"new_position"`, `"old_position"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("comment JSON missing key %s: %s", key, raw)
		}
	}
}

func TestPostComment_HTTP(t *testing.T) {
	var captured commentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widget/issues/42/comments" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		io.WriteString(w, `{"id":99}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "T", srv.Client())
	id, err := c.PostComment(context.Background(), testPR(), "hello world")
	if err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if id != 99 {
		t.Errorf("commentID = %d, want 99", id)
	}
	if captured.Body != "hello world" {
		t.Errorf("body = %q", captured.Body)
	}
}

func TestListReviews_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widget/pulls/42/reviews" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		io.WriteString(w, `[
			{"id":1,"state":"COMMENT","body":"first","stale":true,"user":{"login":"bot","username":"bot"}},
			{"id":2,"state":"APPROVED","body":"","stale":false,"user":{"login":"alice","username":"alice"}}
		]`)
	}))
	defer srv.Close()

	c := New(srv.URL, "T", srv.Client())
	reviews, err := c.ListReviews(context.Background(), testPR())
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("got %d reviews, want 2", len(reviews))
	}
	if reviews[0].ID != 1 || reviews[0].State != "COMMENT" || reviews[0].Body != "first" || !reviews[0].Stale {
		t.Errorf("reviews[0] = %+v", reviews[0])
	}
	if reviews[1].ID != 2 || reviews[1].State != "APPROVED" || reviews[1].Stale {
		t.Errorf("reviews[1] = %+v", reviews[1])
	}
}

func TestDismissReview_HTTP(t *testing.T) {
	var captured dismissRequest
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/repos/acme/widget/pulls/42/reviews/55/dismissals" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "T", srv.Client())
	if err := c.DismissReview(context.Background(), testPR(), 55, "superseded"); err != nil {
		t.Fatalf("DismissReview: %v", err)
	}
	if !hit {
		t.Fatal("server was not hit")
	}
	if captured.Message != "superseded" {
		t.Errorf("message = %q, want superseded", captured.Message)
	}
}

func TestDoJSON_Non2xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"message":"bad position"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "T", srv.Client())
	_, err := c.PostComment(context.Background(), testPR(), "x")
	if err == nil {
		t.Fatal("expected error on non-2xx, got nil")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "bad position") {
		t.Errorf("error should include status and body, got: %v", err)
	}
}

func TestNew_DefaultClientAndBaseURLTrim(t *testing.T) {
	c := New("https://gitea.example.com/", "tok", nil)
	if c.http == nil {
		t.Fatal("expected default http client")
	}
	if c.baseURL != "https://gitea.example.com" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
}
