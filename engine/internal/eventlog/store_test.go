package eventlog

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dharaneswar339/durexec/internal/core"
)

// statusOf keeps the table above readable; core.ExecutionStatus is a string.
func statusOf(s string) core.ExecutionStatus { return core.ExecutionStatus(s) }

func TestVectorLiteral(t *testing.T) {
	tests := []struct {
		name string
		in   []float32
		want string
	}{
		{"nil", nil, "[]"},
		{"empty", []float32{}, "[]"},
		{"single", []float32{0.5}, "[0.5]"},
		{"several", []float32{0.1, 0.2, 0.3}, "[0.1,0.2,0.3]"},
		{"negative", []float32{-1, 0, 1}, "[-1,0,1]"},
		{"no spaces after comma", []float32{1.25, -2.5}, "[1.25,-2.5]"},
		{"shortest round trip", []float32{0.1}, "[0.1]"},
		{"integral values keep no exponent", []float32{100000, 2}, "[100000,2]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VectorLiteral(tc.in); got != tc.want {
				t.Fatalf("VectorLiteral(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A 384-dimension vector is what the schema declares, so the shape of the real
// thing is worth asserting: brackets, and exactly 383 separators.
func TestVectorLiteralFullDimension(t *testing.T) {
	v := make([]float32, 384)
	for i := range v {
		v[i] = float32(i) / 384
	}
	got := VectorLiteral(v)
	if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
		t.Fatalf("literal is not bracketed: %.20q...", got)
	}
	if n := strings.Count(got, ","); n != 383 {
		t.Fatalf("got %d separators, want 383", n)
	}
	if strings.Contains(got, " ") {
		t.Fatal("literal contains a space; pgvector wants none")
	}
}

// Every component must survive the round trip through the text literal, because
// a silently rounded embedding would change similarity scores.
func TestVectorLiteralRoundTrips(t *testing.T) {
	vals := []float32{0.1, 0.123456789, -0.987654321, float32(math.Pi), 1e-7, 1e7}
	for _, want := range vals {
		lit := VectorLiteral([]float32{want})
		var got float32
		if _, err := fmt.Sscanf(lit, "[%g]", &got); err != nil {
			t.Fatalf("cannot parse %q: %v", lit, err)
		}
		if got != want {
			t.Fatalf("round trip of %v gave %v (literal %q)", want, got, lit)
		}
	}
}

func TestUpMigrations(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{
			"down files are ignored",
			[]string{"0001_init.down.sql", "0001_init.up.sql"},
			[]string{"0001_init.up.sql"},
		},
		{
			"lexical order regardless of input order",
			[]string{"0003_c.up.sql", "0001_a.up.sql", "0002_b.up.sql"},
			[]string{"0001_a.up.sql", "0002_b.up.sql", "0003_c.up.sql"},
		},
		{
			"unrelated files are ignored",
			[]string{"README.md", "0001_init.up.sql", "notes.txt", "0001_init.up.sql.bak"},
			[]string{"0001_init.up.sql"},
		},
		{
			"zero padding is what makes ten sort after nine",
			[]string{"0010_j.up.sql", "0009_i.up.sql"},
			[]string{"0009_i.up.sql", "0010_j.up.sql"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := UpMigrations(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("UpMigrations(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("UpMigrations(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{-1, defaultListLimit},
		{0, defaultListLimit},
		{1, 1},
		{50, 50},
		{maxListLimit, maxListLimit},
		{maxListLimit + 1, maxListLimit},
		{1 << 30, maxListLimit},
	}
	for _, tc := range tests {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestListExecutionsQuery(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		limit      int
		wantFilter bool
		wantArgs   int
		wantLimit  string
	}{
		{"no status filter", "", 10, false, 2, "LIMIT $2"},
		{"status filter shifts the limit placeholder", "RUNNING", 10, true, 3, "LIMIT $3"},
		{"unspecified limit is defaulted", "", 0, false, 2, "LIMIT $2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := listExecutionsQuery(7, statusOf(tc.status), tc.limit)

			if got := strings.Contains(sql, "AND status = $2"); got != tc.wantFilter {
				t.Errorf("status filter present = %v, want %v; sql: %s", got, tc.wantFilter, sql)
			}
			if !strings.Contains(sql, tc.wantLimit) {
				t.Errorf("sql %q does not contain %q", sql, tc.wantLimit)
			}
			if len(args) != tc.wantArgs {
				t.Fatalf("got %d args, want %d: %v", len(args), tc.wantArgs, args)
			}
			if args[0] != int32(7) {
				t.Errorf("first arg = %v, want shard 7 as int32", args[0])
			}
			// The limit is always the last placeholder, whatever came before it.
			if got := args[len(args)-1]; got != clampLimit(tc.limit) {
				t.Errorf("last arg = %v, want %d", got, clampLimit(tc.limit))
			}
		})
	}
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, true},
		{"wrapped unique violation", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"}), true},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, false},
		{"serialisation failure", &pgconn.PgError{Code: "40001"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNullHelpers(t *testing.T) {
	if got := nullTime(time.Time{}); got != nil {
		t.Errorf("nullTime(zero) = %v, want nil", got)
	}
	now := time.Now()
	if got := nullTime(now); got != any(now) {
		t.Errorf("nullTime(now) = %v, want %v", got, now)
	}

	if got := nullSeq(0); got != nil {
		t.Errorf("nullSeq(0) = %v, want nil", got)
	}
	if got := nullSeq(5); got != any(int64(5)) {
		t.Errorf("nullSeq(5) = %v, want int64(5)", got)
	}

	if got := nullString(""); got != nil {
		t.Errorf("nullString(\"\") = %v, want nil", got)
	}
	if got := nullString("x"); got != any("x") {
		t.Errorf("nullString(x) = %v, want \"x\"", got)
	}

	// The payload column is NOT NULL, so an absent payload must become the JSON
	// literal null rather than an empty string.
	if got := string(jsonOrNull(nil)); got != "null" {
		t.Errorf("jsonOrNull(nil) = %q, want \"null\"", got)
	}
	if got := string(jsonOrNull([]byte(`{"k":1}`))); got != `{"k":1}` {
		t.Errorf("jsonOrNull passed through wrong: %q", got)
	}
}
