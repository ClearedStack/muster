//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// scanCanary looks for text in every object reachable from any ref of a bare
// repository: every commit (author, committer, message), every tree (file
// names) and every blob of every revision, binary or not, including what a
// later commit deleted. It says where each hit is.
//
// A git failure is an error. Nothing is ever read as "not found" because git
// could not look.
func scanCanary(gitDir, text string) (hits []string, err error) {
	git := func(stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"--git-dir", gitDir}, args...)...)
		cmd.Stdin = bytes.NewReader(stdin)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return out.Bytes(), nil
	}
	if _, err := git(nil, "rev-parse", "--git-dir"); err != nil {
		return nil, err
	}
	listing, err := git(nil, "rev-list", "--all", "--objects")
	if err != nil {
		return nil, err
	}
	var ids bytes.Buffer
	paths := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
		if line == "" {
			continue
		}
		sha, path, _ := strings.Cut(line, " ")
		paths[sha] = path
		ids.WriteString(sha + "\n")
	}
	if ids.Len() == 0 {
		return nil, fmt.Errorf("the repository holds no objects reachable from a ref")
	}
	// cat-file --batch prints "<sha> <type> <size>", then that many bytes
	// of the object as stored (a tree holds its file names), then a newline.
	out, err := git(ids.Bytes(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(bytes.NewReader(out))
	needle := []byte(text)
	for {
		header, err := r.ReadString('\n')
		if err == io.EOF && header == "" {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading git cat-file output: %w", err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("git cat-file said %q", strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file said %q", strings.TrimSpace(header))
		}
		body := make([]byte, size+1) // the object and the newline after it
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("reading a %s object: %w", f[1], err)
		}
		if bytes.Contains(body, needle) {
			hits = append(hits, fmt.Sprintf("%s %.10s (%s)", f[1], f[0], paths[f[0]]))
		}
	}
	return hits, nil
}
