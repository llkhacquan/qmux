package main

import (
	"reflect"
	"testing"
)

// card builds the row sequence the tree emits per Claude pane: an untagged top
// border, content rows tagged with the session, an untagged bottom border.
func card(session, pane string) []Row {
	return []Row{
		{Kind: kindBorderTop},
		{Kind: kindIntent, PaneID: pane, Session: session, Text: "task"},
		{Kind: kindLocation, PaneID: pane, Session: session, Text: "loc"},
		{Kind: kindBorderBot},
	}
}

func rowsFor(cards ...[]Row) []Row {
	var out []Row
	for _, c := range cards {
		out = append(out, c...)
	}
	return out
}

func TestFilterHiddenRows_DropsWholeCardIncludingBorders(t *testing.T) {
	rows := rowsFor(card("nova", "%1"), card("nil", "%2"), card("config", "%3"))
	got := filterHiddenRows(rows, map[string]bool{"nil": true})

	for _, r := range got {
		if r.Session == "nil" {
			t.Fatalf("hidden session nil leaked a content row: %+v", r)
		}
	}
	// nova(4) + config(4) survive; nil's 4 rows (incl. both borders) are gone.
	if len(got) != 8 {
		t.Fatalf("want 8 rows after hiding nil, got %d", len(got))
	}
	// No orphan borders: equal top/bottom counts.
	var tops, bots int
	for _, r := range got {
		switch r.Kind {
		case kindBorderTop:
			tops++
		case kindBorderBot:
			bots++
		}
	}
	if tops != 2 || bots != 2 {
		t.Fatalf("orphan borders: tops=%d bots=%d", tops, bots)
	}
}

func TestFilterHiddenRows_EmptySetIsIdentity(t *testing.T) {
	rows := rowsFor(card("nova", "%1"), card("nil", "%2"))
	got := filterHiddenRows(rows, nil)
	if !reflect.DeepEqual(got, rows) {
		t.Fatalf("empty hidden set must be identity")
	}
}

func TestFilterHiddenRows_AllHidden(t *testing.T) {
	rows := rowsFor(card("nova", "%1"), card("nil", "%2"))
	got := filterHiddenRows(rows, map[string]bool{"nova": true, "nil": true})
	if len(got) != 0 {
		t.Fatalf("want 0 rows when all hidden, got %d", len(got))
	}
}

func TestSummarizeWorkspaces_CountsCardsAndMarksHidden(t *testing.T) {
	// nova has 2 panes, nil has 1. Summarize runs on UNFILTERED rows.
	rows := rowsFor(card("nova", "%1"), card("nova", "%2"), card("nil", "%3"))
	got := summarizeWorkspaces(rows, map[string]bool{"nil": true})

	want := []workspaceItem{
		{Name: "nova", Count: 2, Hidden: false},
		{Name: "nil", Count: 1, Hidden: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summarize mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestMenuRowToWorkspace(t *testing.T) {
	if got := menuRowToWorkspace(2); got != 0 {
		t.Fatalf("first workspace row should map to index 0, got %d", got)
	}
	if got := menuRowToWorkspace(1); got != -1 { // separator line → inert
		t.Fatalf("separator should map to -1, got %d", got)
	}
}

func TestHiddenSetRoundTrip(t *testing.T) {
	set := hiddenCSVToSet(" nova , nil ,,config ")
	want := map[string]bool{"nova": true, "nil": true, "config": true}
	if !reflect.DeepEqual(set, want) {
		t.Fatalf("csv parse mismatch: %+v", set)
	}
	// sortedHiddenSlice is deterministic so the persisted option is stable.
	if got := sortedHiddenSlice(set); !reflect.DeepEqual(got, []string{"config", "nil", "nova"}) {
		t.Fatalf("sorted slice mismatch: %+v", got)
	}
}
