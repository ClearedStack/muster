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

	// Argo CD applies deploy/ through deploy/kustomization.yaml. The top-level
	// kustomization.yaml only points there.
	if !strings.Contains(mustReadText(t, filepath.Join(root, "kustomization.yaml")), "\n  - deploy\n") {
		t.Error("kustomization.yaml does not point at deploy")
	}
	kustomText := mustReadText(t, filepath.Join(root, "deploy/kustomization.yaml"))
	listedSet := map[string]bool{}
	for _, line := range strings.Split(kustomText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") && strings.HasSuffix(trimmed, ".yaml") {
			rel := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			listedSet[rel] = true
			if _, err := os.Stat(filepath.Join(root, "deploy", rel)); err != nil {
				t.Errorf("deploy/kustomization.yaml lists missing file %s: %v", rel, err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "deploy"))
	if err != nil {
		t.Fatalf("reading deploy/: %v", err)
	}
	if len(listedSet) == 0 {
		t.Fatal("deploy/kustomization.yaml lists no manifests")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == "kustomization.yaml" {
			continue
		}
		if !listedSet[entry.Name()] {
			t.Errorf("deploy/%s is not listed in deploy/kustomization.yaml", entry.Name())
		}
		text := mustReadText(t, filepath.Join(root, "deploy", entry.Name()))
		if regexp.MustCompile(`(?m)^kind:\s*Secret\b`).MatchString(text) {
			t.Errorf("deploy/%s defines a Secret in Git; secrets must be generated in-cluster or sealed", entry.Name())
		}
	}

	workload := mustReadText(t, filepath.Join(root, "deploy/muster.yaml"))
	// Muster asks the cluster whose token a caller presented: its
	// ServiceAccount is bound to system:auth-delegator, which allows that.
	for _, want := range []string{
		"kind: ServiceAccount",
		"kind: ClusterRoleBinding",
		"name: system:auth-delegator",
	} {
		if !strings.Contains(workload, want) {
			t.Errorf("deploy/muster.yaml missing %q", want)
		}
	}
	imageLine := regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`).FindStringSubmatch(workload)
	if imageLine == nil {
		t.Fatal("deploy/muster.yaml has no container image line")
	}
	// The repository names the image only; the registry and digest come from
	// the deployer's kustomize image override, so no registry address is here.
	if imageLine[1] != "muster" {
		t.Errorf("image %q: want the bare name muster, overridden at deploy time", imageLine[1])
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
		"port: 8080",
		"serviceAccountName: muster",
		"automountServiceAccountToken: true",
		"key: kubernetes.io/arch",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		"operator: NotIn",
		`values: ["cm3588-plus"]`,
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"type: RuntimeDefault",
		"cpu: 20m",
		"memory: 48Mi",
		"name: muster-database",
		"key: url",
		"secretName: muster-salt",
	} {
		if !strings.Contains(workload, want) {
			t.Errorf("deploy/muster.yaml missing required setting %q", want)
		}
	}

	// The default ownership map in the ConfigMap must pass Muster's parser.
	if _, err := intake.ParseOwnership([]byte(`{"elements":{}}`)); err != nil {
		t.Fatalf("ConfigMap ownership_map.json rejected by intake.ParseOwnership: %v", err)
	}

	// Every literal MUSTER_* variable in deploy/muster.yaml must load cleanly
	// through config.Load. MUSTER_DATABASE_URL comes from Secret
	// muster-database, so it must not be written in Git; a made-up address of
	// the same form stands in for it.
	envFromManifest := extractLiteralEnv(workload)
	if v, ok := envFromManifest["MUSTER_DATABASE_URL"]; ok {
		t.Errorf("MUSTER_DATABASE_URL %q is written in Git; it belongs in Secret muster-database", v)
	}
	envFromManifest["MUSTER_DATABASE_URL"] = "postgres://muster:made-up-secret@logbook.clearedstack.svc.cluster.local:5432/logbook?sslmode=disable"
	cfg, err := config.Load(func(k string) string { return envFromManifest[k] })
	if err != nil {
		t.Fatalf("deploy/muster.yaml environment rejected by config.Load: %v (env=%v)", err, envFromManifest)
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
	// that does not. Workers come in as the whole cs-workers namespace;
	// control-plane callers are named in the podSelector's values list.
	ingressSection, _, ok := strings.Cut(netpol, "  egress:")
	if !ok {
		t.Fatal("deploy/networkpolicy.yaml has no egress section")
	}
	_, ingressSection, ok = strings.Cut(ingressSection, "  ingress:")
	if !ok {
		t.Fatal("deploy/networkpolicy.yaml has no ingress section")
	}
	values := regexp.MustCompile(`values:\s*\[([^\]]*)\]`).FindStringSubmatch(ingressSection)
	if values == nil {
		t.Fatal("deploy/networkpolicy.yaml ingress names no control-plane callers")
	}
	named := map[string]bool{}
	for _, v := range strings.Split(values[1], ",") {
		named[strings.Trim(strings.TrimSpace(v), `"`)] = true
	}
	for _, comp := range registry.All() {
		present := named[comp.Slug]
		if comp.Kind == registry.Worker {
			present = strings.Contains(ingressSection, "kubernetes.io/metadata.name: "+comp.Namespace)
		}
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
	re := regexp.MustCompile(`- name:\s*(MUSTER_[A-Z0-9_]+)\s*\n\s*value:\s*"?([^"\n]*?)"?\s*\n`)
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
