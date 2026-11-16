package api

import (
	"os"
	"regexp"
	"testing"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 活动记录里的每种操作在界面上都要有中英文名称（web/src/i18n/*/activity.ts 的 actions），
// 否则活动页显示的是接口名。新增接口忘了加名称时这里会失败。
func TestActivityLabels(t *testing.T) {
	key := regexp.MustCompile(`(?m)^\s{4}(\w+):`)
	for _, lang := range []string{"zh", "en"} {
		b, err := os.ReadFile("../../../web/src/i18n/" + lang + "/activity.ts")
		if err != nil {
			t.Fatal(err)
		}
		labels := map[string]bool{}
		for _, m := range key.FindAllStringSubmatch(string(b), -1) {
			labels[m[1]] = true
		}
		services := nagiv1.File_nagi_v1_api_proto.Services()
		for i := range services.Len() {
			svc := services.Get(i)
			for j := range svc.Methods().Len() {
				method := string(svc.Methods().Get(j).Name())
				if activityRecorded("/nagi.v1."+string(svc.Name())+"/"+method) && !labels[method] {
					t.Errorf("%s/activity.ts 缺少 %s 的名称", lang, method)
				}
			}
		}
	}
}
