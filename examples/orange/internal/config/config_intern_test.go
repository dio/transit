package config

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInternPool_Intern(t *testing.T) {
	p := NewInternPool()

	id0 := p.Intern("alpha")
	id1 := p.Intern("beta")
	id2 := p.Intern("alpha") // duplicate

	assert.Equal(t, id0, id2, "same string must return same id")
	assert.NotEqual(t, id0, id1, "different strings must return different ids")
}

func TestInternPool_Lookup(t *testing.T) {
	p := NewInternPool()

	id := p.Intern("hello")
	assert.Equal(t, "hello", p.Lookup(id))
}

func TestInternPool_Lookup_OutOfRange(t *testing.T) {
	p := NewInternPool()
	assert.Equal(t, "", p.Lookup(999))
}

func TestInternPool_Roundtrip(t *testing.T) {
	p := NewInternPool()
	words := []string{"workspace", "user", "name", "workspace"} // duplicate at end

	seen := map[string]uint32{}
	for _, w := range words {
		id := p.Intern(w)
		if prev, ok := seen[w]; ok {
			assert.Equal(t, prev, id, "repeated Intern(%q) must return same id", w)
		}
		seen[w] = id
		assert.Equal(t, w, p.Lookup(id), "Lookup must recover original string")
	}
}

func TestInternPool_Concurrent(t *testing.T) {
	p := NewInternPool()
	const workers = 50
	const strings = 20

	var wg sync.WaitGroup
	results := make([][]uint32, workers)

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ids := make([]uint32, strings)
			for i := range strings {
				ids[i] = p.Intern(fmt.Sprintf("str-%02d", i))
			}
			results[w] = ids
		}(w)
	}
	wg.Wait()

	// All workers must agree on every id.
	for i := range strings {
		id := results[0][i]
		for w := 1; w < workers; w++ {
			assert.Equal(t, id, results[w][i], "worker %d disagreed on id for str-%02d", w, i)
		}
	}

	// Every interned string must round-trip.
	for i := range strings {
		assert.Equal(t, fmt.Sprintf("str-%02d", i), p.Lookup(results[0][i]))
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		wantErr   bool
		workspace string
		user      string
		namePart  string
	}{
		{
			name:      "valid",
			id:        "demo/adi/sk-001",
			workspace: "demo", user: "adi", namePart: "sk-001",
		},
		{
			name:      "valid with hyphens",
			id:        "org-a/alice/my-key",
			workspace: "org-a", user: "alice", namePart: "my-key",
		},
		{
			name:    "one segment",
			id:      "demo",
			wantErr: true,
		},
		{
			name:    "two segments",
			id:      "demo/adi",
			wantErr: true,
		},
		{
			name:    "four segments",
			id:      "demo/adi/sk/extra",
			wantErr: true,
		},
		{
			name:    "empty workspace",
			id:      "/adi/sk-001",
			wantErr: true,
		},
		{
			name:    "empty user",
			id:      "demo//sk-001",
			wantErr: true,
		},
		{
			name:    "empty name",
			id:      "demo/adi/",
			wantErr: true,
		},
		{
			name:    "empty string",
			id:      "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInternPool()
			got, err := parseID(tc.id, p)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.id, got.Raw)
			assert.Equal(t, tc.workspace, p.Lookup(got.Workspace))
			assert.Equal(t, tc.user, p.Lookup(got.User))
			assert.Equal(t, tc.namePart, p.Lookup(got.Name))
		})
	}
}

func TestParseID_InternsAreShared(t *testing.T) {
	p := NewInternPool()

	a, err := parseID("demo/adi/sk-1", p)
	require.NoError(t, err)
	b, err := parseID("demo/adi/sk-2", p)
	require.NoError(t, err)

	assert.Equal(t, a.Workspace, b.Workspace, "same workspace must share intern id")
	assert.Equal(t, a.User, b.User, "same user must share intern id")
	assert.NotEqual(t, a.Name, b.Name)
}
