package api

import (
	"strings"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func TestActivityRecorded(t *testing.T) {
	for procedure, want := range map[string]bool{
		nagiv1connect.ProjectServiceDeployProcedure:           true,
		nagiv1connect.ProjectServiceControlContainerProcedure: true,
		nagiv1connect.AuthServiceLoginProcedure:               false, // 在 auth 里记
		nagiv1connect.ProjectServiceListVersionsProcedure:     false,
		nagiv1connect.ProjectServiceGetProjectProcedure:       true, // 带着变量值，按会话每小时记一条
		nagiv1connect.ProjectServiceGetBackupKeyProcedure:     true,
		nagiv1connect.AuthServiceMeProcedure:                  false,
		nagiv1connect.SettingsServiceListActivityProcedure:    false,
	} {
		if got := activityRecorded(procedure); got != want {
			t.Errorf("%s: 记录 = %v，应为 %v", procedure, got, want)
		}
	}
}

// 记录只取固定字段：登录请求里只有用户名，密码读不到。
func TestActivityFieldNeverReadsSecrets(t *testing.T) {
	req := &nagiv1.LoginRequest{Username: "admin", Password: "secret"}
	if got := activityField(req, "username"); got != "admin" {
		t.Errorf("用户名 = %q", got)
	}
	if got := activityField(req, "project_id", "container", "name", "node_id", "id"); got != "" {
		t.Errorf("目标 = %q，应为空", got)
	}
}

func TestActivityFieldFirstMatch(t *testing.T) {
	req := &nagiv1.ControlContainerRequest{ProjectId: "p1", Container: "web"}
	if got := activityField(req, "project_id"); got != "p1" {
		t.Errorf("项目 = %q", got)
	}
	if got := activityField(req, "container", "name"); got != "web" {
		t.Errorf("目标 = %q", got)
	}
}

// notSensitiveOutputs 是回复里有名字像密钥的字段、但不用记成敏感查询的接口，写明原因。
var notSensitiveOutputs = map[string]string{
	"GetNodeMesh":       "WireGuard 公钥",
	"GetPanelMesh":      "WireGuard 公钥",
	"InspectComposeDir": "secret_variables 只是变量名",
	"GetSettings":       "master_key_path 是文件路径",
	"ListAuditEvents":   "token_name 是令牌的名称",
	"GetAuditEvent":     "token_name 是令牌的名称",
	"GetAuditSettings":  "外送目标的 Secret Key 和请求头的值返回时留空",
}

// 返回密钥、变量值或文件内容的查询都要登记在 store.SensitiveReads（见 docs/audit.md#记录范围）。
// 修改类的接口本来就会记，不用登记。
func TestSensitiveReadsRegistered(t *testing.T) {
	fd, err := protoregistry.GlobalFiles.FindFileByPath("nagi/v1/api.proto")
	if err != nil {
		t.Fatal(err)
	}
	words := []string{"password", "token", "secret", "key", "value", "content", "data"}
	for i := range fd.Services().Len() {
		svc := fd.Services().Get(i)
		for j := range svc.Methods().Len() {
			m := svc.Methods().Get(j)
			procedure := "/" + string(svc.FullName()) + "/" + string(m.Name())
			if !activityRecordedAsRead(procedure) {
				continue
			}
			var hits []string
			seen := map[protoreflect.FullName]bool{}
			var walk func(md protoreflect.MessageDescriptor)
			walk = func(md protoreflect.MessageDescriptor) {
				if seen[md.FullName()] {
					return
				}
				seen[md.FullName()] = true
				for k := range md.Fields().Len() {
					f := md.Fields().Get(k)
					kind := f.Kind()
					if f.IsMap() {
						kind = f.MapValue().Kind()
					}
					if kind == protoreflect.StringKind || kind == protoreflect.BytesKind {
						for _, w := range words {
							if strings.Contains(string(f.Name()), w) {
								hits = append(hits, string(f.FullName()))
								break
							}
						}
					}
					if f.IsMap() {
						if mm := f.MapValue().Message(); mm != nil {
							walk(mm)
						}
					} else if mm := f.Message(); mm != nil {
						walk(mm)
					}
				}
			}
			walk(m.Output())
			name := string(m.Name())
			if len(hits) == 0 || store.SensitiveReads[name] {
				continue
			}
			if _, ok := notSensitiveOutputs[name]; ok {
				continue
			}
			t.Errorf("%s 的回复里有 %v：登记到 store.SensitiveReads，或在 notSensitiveOutputs 里写明为什么不用记", name, hits)
		}
	}
}

// activityRecordedAsRead 判断接口是不记录的查询。
func activityRecordedAsRead(procedure string) bool {
	method := methodOf(procedure)
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}
