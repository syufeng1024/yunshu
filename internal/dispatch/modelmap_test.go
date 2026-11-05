package dispatch

import (
	"testing"

	"yunshu.dev/core/internal/gw"
	"yunshu.dev/core/internal/jsonx"
)

// 这两个用例钉住的是分发侧的上游模型名决议规则：
//
//	route.upstream_model_name        该渠道的默认上游名（物理绑定）
//	channel.upstream_model_name_map  按平台模型名的覆盖（逻辑映射，优先）
//
// 覆盖层不是装饰：同一个平台模型名在不同渠道要发成不同的上游名（官方直连叫
// deepseek-flash、某云叫 deepseek-v3），以及厂商改名后保住对外名字不变，都靠它。
// 谁要删这段逻辑，必须先删掉这两条测试——那应当是一次有意识的决定，而不是顺手清理。
func buildUpstreamModel(t *testing.T, ch *gw.ChannelEntry, route gw.RouteCandidate) string {
	t.Helper()
	d := &Dispatcher{}
	body, _, err := d.buildBody(testCtx(false), target{route: route, ch: ch})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := jsonx.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v (%s)", err, body)
	}
	return payload.Model
}

func TestChannelModelMapOverridesRouteUpstreamName(t *testing.T) {
	ch := &gw.ChannelEntry{
		ID: 10, ProtocolID: 2, StreamUsageMode: "UNKNOWN",
		UpstreamModelNameMap: map[string]string{"m": "mapped-model"},
	}
	route := gw.RouteCandidate{RouteID: 1, ChannelID: 10, ProtocolID: 2, UpstreamModelName: "route-default-model"}

	if got := buildUpstreamModel(t, ch, route); got != "mapped-model" {
		t.Fatalf("upstream model = %q, want mapped-model (map must win over the route default)", got)
	}
}

func TestChannelModelMapFallsBackToRouteUpstreamName(t *testing.T) {
	// 映射只覆盖它自己声明的键；其余模型直接用路由上的默认名，这正是「模型名与
	// 厂商官方名称一致时不用写映射」的实现依据。
	ch := &gw.ChannelEntry{
		ID: 10, ProtocolID: 2, StreamUsageMode: "UNKNOWN",
		UpstreamModelNameMap: map[string]string{"other-model": "other-upstream"},
	}
	route := gw.RouteCandidate{RouteID: 1, ChannelID: 10, ProtocolID: 2, UpstreamModelName: "route-default-model"}

	if got := buildUpstreamModel(t, ch, route); got != "route-default-model" {
		t.Fatalf("upstream model = %q, want route-default-model", got)
	}
}

func TestChannelModelMapAppliesToStreamingToo(t *testing.T) {
	// 流式与非流式共用同一份决议；映射只在 buildBody 里做一次，不存在两条路径
	// 各写一遍导致漂移的可能。
	ch := &gw.ChannelEntry{
		ID: 10, ProtocolID: 2, StreamUsageMode: "UNKNOWN",
		UpstreamModelNameMap: map[string]string{"m": "mapped-model"},
	}
	route := gw.RouteCandidate{RouteID: 1, ChannelID: 10, ProtocolID: 2, UpstreamModelName: "route-default-model"}

	d := &Dispatcher{}
	body, _, err := d.buildBody(testCtx(true), target{route: route, ch: ch})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var payload struct {
		Model         string `json:"model"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := jsonx.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v (%s)", err, body)
	}
	if payload.Model != "mapped-model" {
		t.Fatalf("stream upstream model = %q, want mapped-model", payload.Model)
	}
}
