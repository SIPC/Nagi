package agent

import (
	"testing"

	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 节点访问的接口都不能算查询：备用面板不能调用（见 checkPrimary）。新增接口时加到这里。
func TestNodeAccessNotReadOnly(t *testing.T) {
	for _, m := range []string{
		nagiv1.AgentService_NodeShell_FullMethodName,
		nagiv1.AgentService_ListNodeDir_FullMethodName,
		nagiv1.AgentService_ReadNodeFile_FullMethodName,
		nagiv1.AgentService_WriteNodeFile_FullMethodName,
		nagiv1.AgentService_DeleteNodeFile_FullMethodName,
		nagiv1.AgentService_CreateNodeDir_FullMethodName,
		nagiv1.AgentService_ListNodeFileBackups_FullMethodName,
		nagiv1.AgentService_RestoreNodeFileBackup_FullMethodName,
		nagiv1.MeshService_NodeFileDownload_FullMethodName,
		nagiv1.MeshService_NodeFileUpload_FullMethodName,
	} {
		if readOnlyRPCs[m] {
			t.Errorf("%s 不能放行给备用面板", m)
		}
	}
}

// mesh 上有别的节点：只有面板的公钥才算面板。
func TestPanelOf(t *testing.T) {
	var panel, other mesh.Key
	panel[0], other[0] = 1, 2
	n := &meshNode{panelKeys: map[string]mesh.Key{"fp": panel}}
	if fp, ok := n.panelOf(panel); !ok || fp != "fp" {
		t.Fatalf("面板：%q %v", fp, ok)
	}
	if _, ok := n.panelOf(other); ok {
		t.Fatal("别的节点不能算面板")
	}
}
