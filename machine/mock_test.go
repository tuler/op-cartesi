package machine

import (
	"bytes"
	"context"
	"testing"
)

func TestMockDeterminism(t *testing.T) {
	ctx := context.Background()
	a := NewMock([]byte("seed"))
	b := NewMock([]byte("seed"))
	inputs := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for _, in := range inputs {
		if _, err := a.AdvanceInput(ctx, in, 1<<30); err != nil {
			t.Fatal(err)
		}
		if _, err := b.AdvanceInput(ctx, in, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	ra, _ := a.RootHash(ctx)
	rb, _ := b.RootHash(ctx)
	if ra != rb {
		t.Fatalf("same inputs produced different roots: %s vs %s", ra, rb)
	}
}

func TestMockForkIsolation(t *testing.T) {
	ctx := context.Background()
	parent := NewMock([]byte("seed"))
	if _, err := parent.AdvanceInput(ctx, []byte("shared"), 1<<30); err != nil {
		t.Fatal(err)
	}
	before, _ := parent.RootHash(ctx)

	child, err := parent.Fork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.AdvanceInput(ctx, []byte("child-only"), 1<<30); err != nil {
		t.Fatal(err)
	}
	after, _ := parent.RootHash(ctx)
	if before != after {
		t.Fatal("advancing the fork mutated the parent")
	}
	cr, _ := child.RootHash(ctx)
	if cr == after {
		t.Fatal("fork root did not change after input")
	}
}

func TestMockRejectAndCycleLimit(t *testing.T) {
	ctx := context.Background()
	m := NewMock([]byte("seed"))
	m.RejectFn = func(input []byte) bool { return bytes.Contains(input, []byte("REJECT")) }

	before, _ := m.RootHash(ctx)
	res, err := m.AdvanceInput(ctx, []byte("please REJECT me"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted {
		t.Fatal("expected rejection")
	}
	if after, _ := m.RootHash(ctx); after != before {
		t.Fatal("rejected input mutated state")
	}

	if _, err := m.AdvanceInput(ctx, []byte("fine"), 1); err != ErrCycleLimit {
		t.Fatalf("expected ErrCycleLimit, got %v", err)
	}
	if after, _ := m.RootHash(ctx); after != before {
		t.Fatal("over-budget input mutated state")
	}
}
