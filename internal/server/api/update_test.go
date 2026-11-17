package api

import (
	"context"
	"testing"

	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
)

// 备用面板只读，但要能查新版本、更新自己：两边数据库版本不同时同步会暂停，只能在备用面板上解决。
func TestStandbyCanUpdateItself(t *testing.T) {
	for _, p := range []string{
		nagiv1connect.SettingsServiceCheckPanelUpdateProcedure,
		nagiv1connect.SettingsServiceUpdatePanelProcedure,
		nagiv1connect.SettingsServiceGetPanelReleaseNotesProcedure,
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

// 经加速代理查到的最新版本记下是哪个代理，缓存期内也带着，界面据此写明「查询自 <代理>」；
// 之后直接从 GitHub 查到时清掉。
func TestLatestCheckedVia(t *testing.T) {
	old := latestFunc
	t.Cleanup(func() { latestFunc = old })
	via := "ghfast.top"
	latestFunc = func(context.Context, string) (string, string, error) { return "v0.99.0", via, nil }
	var u panelUpdates
	c, err := u.getChecked(context.Background(), false, "")
	if err != nil || c.version != "v0.99.0" || c.via != "ghfast.top" {
		t.Fatalf("得到 %+v、%v", c, err)
	}
	via = ""
	if c, _ := u.getChecked(context.Background(), false, ""); c.via != "ghfast.top" {
		t.Fatalf("缓存的结果应带着查询的代理：%+v", c)
	}
	if c, _ := u.getChecked(context.Background(), true, ""); c.via != "" {
		t.Fatalf("直接从 GitHub 查到时应清掉：%+v", c)
	}
}
