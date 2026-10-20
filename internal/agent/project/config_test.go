package project

import (
	"context"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/compose-spec/compose-go/v2/dotenv"
)

// 写出去的 .env 必须被 compose 原样读回来。
func TestEnvFileRoundTrip(t *testing.T) {
	values := map[string]string{
		"PLAIN":    "hello",
		"SPACES":   "a b  c",
		"DOLLAR":   "pa$$word${NOT_A_VAR}",
		"SINGLE":   "it's",
		"DOUBLE":   `say "hi"`,
		"MIXED":    `it's "$HOME" \ back`,
		"NEWLINE":  "line1\nline2",
		"HASH":     "a#b # c",
		"EMPTY":    "",
		"UNICODE":  "凪 Nagi",
		"EQUALS":   "a=b=c",
		"BACKTICK": "`cmd`",
	}
	var vars []*nagiv1.EnvVar
	for k, v := range values {
		vars = append(vars, &nagiv1.EnvVar{Key: k, Value: v})
	}
	got, err := dotenv.UnmarshalBytesWithLookup(envFile(vars), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, envFile(vars))
	}
	for k, want := range values {
		if got[k] != want {
			t.Errorf("%s: got %q, want %q", k, got[k], want)
		}
	}
}

// 单个容器只加入项目内网；容器组用自己的默认网络，项目内网由节点接入。
func TestOverrideNetworks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp := containerPaths{root: dir}
	single, err := spec.Load(ctx, &nagiv1.ContainerConfig{Form: &nagiv1.ImageForm{Image: "nginx"}}, "web", nil, dir, "nagi-p-web")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := buildOverride(overrideInput{proj: single, mp: mp, projectID: "p", container: "web", version: 1, single: true})
	if err != nil {
		t.Fatal(err)
	}
	nets := ov["networks"].(map[string]any)
	if n, ok := nets["nagi"].(map[string]any); !ok || n["name"] != "nagi-p" || n["external"] != true {
		t.Fatalf("single container networks: %v", nets)
	}
	svc := ov["services"].(map[string]any)["web"].(map[string]any)
	if svc["networks"] == nil || svc["labels"].(map[string]string)[LabelSingle] != "1" {
		t.Fatalf("single container service: %v", svc)
	}

	compose, err := spec.LoadCompose(ctx, "services:\n  db:\n    image: postgres\n", nil, dir, "nagi-p-umami")
	if err != nil {
		t.Fatal(err)
	}
	ov, err = buildOverride(overrideInput{proj: compose, mp: mp, projectID: "p", container: "umami", version: 1, subnet: "10.200.0.64/26"})
	if err != nil {
		t.Fatal(err)
	}
	nets = ov["networks"].(map[string]any)
	if _, ok := nets["default"]; !ok || nets["nagi"] != nil {
		t.Fatalf("compose container networks: %v", nets)
	}
	if ov["services"].(map[string]any)["db"].(map[string]any)["networks"] != nil {
		t.Fatal("compose container must not join the project network in compose")
	}
}
