package project

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SIPC/nagi/internal/agent/store/nodestore"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 容器目录里由节点按面板下发的配置生成的文件：compose.yml、.env、shared.env、env/<服务>.env、
// docker/config.json、ssh/deploy_key。每次部署时重写，手动修改会被覆盖。

// writeConfigFiles 把容器一个版本的配置写成 compose 需要的文件。shared 是这个版本用的共享变量。
func writeConfigFiles(p containerPaths, cfg *nagiv1.ContainerConfig, container string, shared []*nagiv1.EnvVar) error {
	text, err := spec.ComposeText(cfg, container)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.root, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(p.composeFile(), []byte(text), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(p.envFile(), envFile(cfg.GetEnv()), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(p.sharedEnvFile(), envFile(shared), 0o600); err != nil {
		return err
	}
	// 重新生成服务级环境变量文件，删掉不再需要的。
	if err := os.RemoveAll(p.serviceEnvDir()); err != nil {
		return err
	}
	senv := spec.ServiceEnv(cfg, container)
	if len(senv) == 0 {
		return nil
	}
	if err := os.MkdirAll(p.serviceEnvDir(), 0o700); err != nil {
		return err
	}
	for name, vars := range senv {
		if err := writeFileAtomic(p.serviceEnvFile(name), envFile(vars), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// writeSecrets 写入私有仓库认证和 git 的 deploy key。
func writeSecrets(p containerPaths, s *nagiv1.ContainerSpec) error {
	if cfg := s.GetDockerConfig(); len(cfg) > 0 {
		if err := os.MkdirAll(p.dockerConfigDir(), 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(p.dockerConfigDir(), "config.json"), cfg, 0o600); err != nil {
			return err
		}
	} else if err := os.RemoveAll(p.dockerConfigDir()); err != nil {
		return err
	}
	if key := s.GetDeployKey(); len(key) > 0 {
		if err := os.MkdirAll(p.sshDir(), 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(p.deployKey(), key, 0o600); err != nil {
			return err
		}
		if err := writeFileAtomic(p.deployKeyPub(), []byte(s.GetDeployKeyPub()+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// envFile 生成 compose 能读的 .env 内容。值统一用单引号包起来，原样保留，不做变量替换；
// 值里本身有单引号时改用双引号并转义。
func envFile(vars []*nagiv1.EnvVar) []byte {
	sorted := append([]*nagiv1.EnvVar(nil), vars...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].GetKey() < sorted[j].GetKey() })
	var b strings.Builder
	for _, v := range sorted {
		b.WriteString(v.GetKey())
		b.WriteByte('=')
		b.WriteString(quoteEnv(v.GetValue()))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func quoteEnv(v string) string {
	if !strings.ContainsAny(v, "'\n\r") {
		return "'" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "$", "$$")
	return `"` + r.Replace(v) + `"`
}

func (p containerPaths) serviceEnvDir() string { return filepath.Join(p.root, "env") }
func (p containerPaths) serviceEnvFile(service string) string {
	return filepath.Join(p.serviceEnvDir(), service+".env")
}

func setFilesRoot(s *nagiv1.ContainerSpec, root string) nodestore.SetContainerFilesRootParams {
	return nodestore.SetContainerFilesRootParams{FilesRoot: root, ProjectID: s.GetProjectId(), Name: s.GetName()}
}
