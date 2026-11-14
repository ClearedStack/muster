package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ClearedStack/muster/internal/auth"
	"github.com/ClearedStack/muster/internal/config"
	"github.com/ClearedStack/muster/internal/intake"
	"github.com/ClearedStack/muster/internal/logbook/sql"
	"github.com/ClearedStack/muster/internal/registry"
)

func TestDeployManifests(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	kustomText := mustReadText(t, filepath.Join(root, "kustomization.yaml"))
	var listed []string
	for _, line := range strings.Split(kustomText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- deploy/") {
			rel := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			listed = append(listed, rel)
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				t.Errorf("kustomization.yaml lists missing file %s: %v", rel, err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "deploy"))
	if err != nil {
		t.Fatalf("reading deploy/: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("deploy/ holds no manifests")
	}
	listedSet := map[string]bool{}
	for _, rel := range listed {
		listedSet[rel] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		rel := "deploy/" + entry.Name()
		if !listedSet[rel] {
			t.Errorf("%s is not listed in kustomization.yaml", rel)
		}
		text := mustReadText(t, filepath.Join(root, rel))
		if regexp.MustCompile(`(?m)^kind:\s*Secret\b`).MatchString(text) {
			t.Errorf("%s defines a Secret in Git; secrets must be generated in-cluster or sealed", rel)
		}
	}

	rbac := mustReadText(t, filepath.Join(root, "deploy/rbac.yaml"))
	for _, want := range []string{
		"kind: ServiceAccount",
		"name: muster",
		"namespace: clearedstack",
		"kind: ClusterRole",
		"name: muster-tokenreview",
		`apiGroups: ["authentication.k8s.io"]`,
		`resources: ["tokenreviews"]`,
		`verbs: ["create"]`,
		"kind: ClusterRoleBinding",
	} {
		if !strings.Contains(rbac, want) {
			t.Errorf("deploy/rbac.yaml missing %q", want)
		}
	}

	workload := mustReadText(t, filepath.Join(root, "deploy/muster.yaml"))
	imageLine := regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`).FindStringSubmatch(workload)
	if imageLine == nil {
		t.Fatal("deploy/muster.yaml has no container image line")
	}
	wantImage := regexp.MustCompile(`^lazarette\.clearedstack\.svc\.cluster\.local:5000/muster@sha256:[0-9a-f]{64}$`)
	if !wantImage.MatchString(imageLine[1]) {
		t.Errorf("image %q is not pinned by sha256 digest on lazarette", imageLine[1])
	}
	if strings.Contains(workload, "imagePullPolicy: Always") {
		t.Error("deploy/muster.yaml must not pull from outside at runtime (imagePullPolicy: Always)")
	}
	for _, forbidden := range []string{"LoadBalancer", "NodePort"} {
		if strings.Contains(workload, forbidden) {
			t.Errorf("deploy/muster.yaml exposes Service as %s; must stay ClusterIP", forbidden)
		}
	}
	for _, want := range []string{
		"namespace: clearedstack",
		"type: ClusterIP",
		"port: 80",
		"port: 8080",
		"serviceAccountName: muster",
		"automountServiceAccountToken: true",
		"key: kubernetes.io/arch",
		"- arm64",
		"key: kubernetes.io/hostname",
		"operator: NotIn",
		"- cm3588-plus",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"privileged: false",
		"drop:\n                - ALL",
		"type: RuntimeDefault",
		"cpu: 20m",
		"memory: 48Mi",
		"name: logbook-credentials",
		"key: muster-password",
		"secretName: muster-credentials",
	} {
		if !strings.Contains(workload, want) {
			t.Errorf("deploy/muster.yaml missing required setting %q", want)
		}
	}

	// The default ownership map in the ConfigMap must pass Muster's parser.
	if _, err := intake.ParseOwnership([]byte(`{"elements":{}}`)); err != nil {
		t.Fatalf("ConfigMap ownership_map.json rejected by intake.ParseOwnership: %v", err)
	}

	// Every MUSTER_* variable in deploy/muster.yaml must load cleanly through
	// config.Load, and MUSTER_DATABASE_URL must carry no inline password.
	envFromManifest := extractLiteralEnv(workload)
	envFromManifest["MUSTER_DATABASE_PASSWORD"] = "made-up-secret-@:/?#%"
	cfg, err := config.Load(func(k string) string { return envFromManifest[k] })
	if err != nil {
		t.Fatalf("deploy/muster.yaml environment rejected by config.Load: %v (env=%v)", err, envFromManifest)
	}
	if strings.Contains(cfg.DatabaseURL, "muster:") {
		t.Errorf("MUSTER_DATABASE_URL %q embeds a password in Git", cfg.DatabaseURL)
	}
	store, err := sql.OpenWithPassword(cfg.DatabaseURL, cfg.DatabasePassword, time.Now)
	if err != nil {
		t.Fatalf("MUSTER_DATABASE_URL rejected by sql.OpenWithPassword: %v", err)
	}
	store.Close(t.Context())

	netpol := mustReadText(t, filepath.Join(root, "deploy/networkpolicy.yaml"))
	for _, want := range []string{
		"kind: NetworkPolicy",
		"namespace: clearedstack",
		"- Ingress",
		"- Egress",
		"kubernetes.io/metadata.name: cs-workers",
		"kubernetes.io/metadata.name: clearedstack",
		"kubernetes.io/metadata.name: kube-system",
		"app.kubernetes.io/name: logbook",
		"port: 8080",
		"port: 5432",
		"port: 53",
	} {
		if !strings.Contains(netpol, want) {
			t.Errorf("deploy/networkpolicy.yaml missing %q", want)
		}
	}
	for _, wideOpen := range []string{"0.0.0.0/0", "::/0"} {
		if strings.Contains(netpol, wideOpen) {
			t.Errorf("deploy/networkpolicy.yaml opens %s", wideOpen)
		}
	}
	// Ingress opens every component that may call Muster, and no component
	// that does not.
	ingressSection, _, ok := strings.Cut(netpol, "  egress:")
	if !ok {
		t.Fatal("deploy/networkpolicy.yaml has no egress section")
	}
	_, ingressSection, ok = strings.Cut(ingressSection, "  ingress:")
	if !ok {
		t.Fatal("deploy/networkpolicy.yaml has no ingress section")
	}
	for _, comp := range registry.All() {
		entry := "- " + comp.Slug
		present := strings.Contains(ingressSection, entry)
		allowed := auth.MayCall(comp.Slug)
		if allowed && !present {
			t.Errorf("deploy/networkpolicy.yaml ingress omits caller %s", comp.Slug)
		}
		if !allowed && present {
			t.Errorf("deploy/networkpolicy.yaml ingress opens non-caller %s", comp.Slug)
		}
	}
}

func TestBuildConfigAndNoEmDashes(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	koText := mustReadText(t, filepath.Join(root, ".ko.yaml"))
	for _, want := range []string{"main: ./cmd/muster", "CGO_ENABLED=0"} {
		if !strings.Contains(koText, want) {
			t.Errorf(".ko.yaml missing %q", want)
		}
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		base := strings.ToLower(d.Name())
		if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") {
			t.Errorf("unexpected Dockerfile at %s; images are built with ko", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) {
			return nil
		}
		if strings.ContainsRune(string(data), '\u2014') {
			t.Errorf("%s contains an em dash (U+2014)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func extractLiteralEnv(manifest string) map[string]string {
	out := map[string]string{}
	re := regexp.MustCompile(`- name:\s*(MUSTER_[A-Z0-9_]+)\s*\n\s*value:\s*"([^"]*)"`)
	for _, m := range re.FindAllStringSubmatch(manifest, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func mustReadText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
