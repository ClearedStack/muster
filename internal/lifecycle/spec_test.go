package lifecycle

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The tables in the contract and the tables in this package must say the
// same thing. If one is changed without the other, these tests fail.

const contractFile = "../../contract/v1/CONTRACT.md"

func specSection(t *testing.T, heading string) [][]string {
	t.Helper()
	data, err := os.ReadFile(contractFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	at := strings.Index(text, heading+"\n")
	if at < 0 {
		t.Fatalf("the contract has no heading %q", heading)
	}
	var rows [][]string
	for _, line := range strings.Split(text[at+len(heading):], "\n")[1:] {
		if strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	return rows
}

var ticked = regexp.MustCompile("`([^`]+)`")

func ticks(cell string) []string {
	var out []string
	for _, m := range ticked.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestContractStateTableMatches(t *testing.T) {
	var spec []string
	for _, row := range specSection(t, "## Item states and moves") {
		from := ticks(row[0])
		if len(row) != 2 || len(from) != 1 {
			continue // the header and the rule under it
		}
		switch {
		case row[1] == "nowhere":
		case strings.HasPrefix(row[1], "the state it was paused from"):
			for _, s := range States(KindItem) {
				if Pausable(s) {
					spec = append(spec, from[0]+" > "+string(s))
				}
			}
		default:
			for _, to := range ticks(row[1]) {
				spec = append(spec, from[0]+" > "+to)
			}
		}
	}
	var code []string
	for _, m := range Moves(KindItem) {
		code = append(code, string(m.From)+" > "+string(m.To))
	}
	same(t, "item moves", spec, code)
}

func routeLine(r Route) string {
	next := r.Next
	if next == "" {
		next = "none"
	}
	return fmt.Sprintf("%s: %s > %s, then %s, lane %q, after %d days", r.Worker, r.From, r.To, next, r.Lane, r.AfterDays)
}

var daysLater = regexp.MustCompile(`([0-9]+) days later`)

func specRoutes(t *testing.T, heading string, kind Kind) []string {
	t.Helper()
	var out []string
	for _, row := range specSection(t, heading) {
		if len(row) != 4 || len(ticks(row[1])) != 1 || len(ticks(row[2])) < 1 {
			continue // a header, or a table of another shape
		}
		worker := strings.ToLower(row[0])
		next := strings.ToLower(strings.TrimRight(strings.Fields(row[3])[0], ","))
		lane := Lane("")
		if l := ticks(row[3]); len(l) == 1 {
			lane = Lane(l[0])
		}
		r := Route{Worker: worker, Kind: kind, From: State(ticks(row[1])[0]), To: State(ticks(row[2])[0]), Lane: lane}
		if next != "none" {
			r.Next = next
		}
		if m := daysLater.FindStringSubmatch(row[3]); m != nil {
			r.AfterDays, _ = strconv.Atoi(m[1])
		}
		out = append(out, routeLine(r))
	}
	return out
}

func TestContractRouteTablesMatch(t *testing.T) {
	code := map[Kind][]string{}
	for _, r := range Routes() {
		code[r.Kind] = append(code[r.Kind], routeLine(r))
	}
	same(t, "item queue steps", specRoutes(t, "## Queue steps", KindItem), code[KindItem])
	same(t, "proposal queue steps", specRoutes(t, "## Proposal steps", KindProposal), code[KindProposal])
}

func same(t *testing.T, what string, spec, code []string) {
	t.Helper()
	if len(spec) == 0 {
		t.Fatalf("%s: nothing was read from the contract", what)
	}
	sort.Strings(spec)
	sort.Strings(code)
	if strings.Join(spec, "\n") != strings.Join(code, "\n") {
		t.Errorf("%s: the contract and the code disagree\ncontract:\n  %s\ncode:\n  %s",
			what, strings.Join(spec, "\n  "), strings.Join(code, "\n  "))
	}
}
