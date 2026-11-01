package api

import (
	"testing"

	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 备用面板只读，但要能查新版本、更新自己：两边数据库版本不同时同步会暂停，只能在备用面板上解决。
func TestStandbyCanUpdateItself(t *testing.T) {
	for _, p := range []string{
		nagiv1connect.SettingsServiceCheckPanelUpdateProcedure,
		nagiv1connect.SettingsServiceUpdatePanelProcedure,
	} {
		if !readOnlyProcedure(p) {
			t.Errorf("%s 在备用面板上应该放行", p)
		}
	}
}

func TestUpdateUnsupported(t *testing.T) {
	if err := (&PanelInfo{Version: "dev"}).updateUnsupported(); err == nil {
		t.Error("本地编译的版本不能更新")
	}
}
