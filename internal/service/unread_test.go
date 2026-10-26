package service

import "testing"

// TestSummarizeUnreadSortsByCountDesc 锁住「按未读降序」这个契约。
//
// 客户端拿 items 的第一项决定「点开哪个会话」，顺序错了就是让人先去处理
// 只有一条未读的会话，真正的红点堆在旁边。
func TestSummarizeUnreadSortsByCountDesc(t *testing.T) {
	counts := map[string]int64{
		"p_1_2": 2,
		"p_1_3": 17,
		"p_1_4": 5,
	}

	total, items := summarizeUnread(counts)
	if total != 24 {
		t.Fatalf("总数应为 24，实际 %d", total)
	}
	want := []string{"p_1_3", "p_1_4", "p_1_2"}
	if len(items) != len(want) {
		t.Fatalf("期望 %d 项，实际 %d 项: %v", len(want), len(items), items)
	}
	for i, convID := range want {
		if items[i].ConvID != convID {
			t.Fatalf("第 %d 项应为 %s，实际 %s（完整顺序 %v）", i, convID, items[i].ConvID, items)
		}
	}
}

// TestSummarizeUnreadBreaksTieByConvID 锁住同未读时的稳定顺序。
//
// map 遍历顺序是随机的，少了 conv_id 兜底，同一份数据两次请求返回的顺序
// 可能不同 —— 前端列表会无端重排，看着像在闪。
func TestSummarizeUnreadBreaksTieByConvID(t *testing.T) {
	counts := map[string]int64{
		"p_1_9": 3,
		"p_1_2": 3,
		"p_1_5": 3,
	}

	for round := 0; round < 20; round++ {
		_, items := summarizeUnread(counts)
		for i := 1; i < len(items); i++ {
			if items[i-1].ConvID >= items[i].ConvID {
				t.Fatalf("同未读应按 conv_id 升序，实际 %v", items)
			}
		}
	}
}

// TestSummarizeUnreadSkipsNonPositive 验证 0 与负数不计入总数、不进列表。
//
// 未读清零走 HDEL，正常不会有 0 值；但只要有过「清零改成 HSET 0」的实现，
// 这里就会立刻暴露成「列表里一堆 0 未读」。空结果必须是非 nil 切片，
// 否则 JSON 会序列化成 null，前端 .map 直接炸。
func TestSummarizeUnreadSkipsNonPositive(t *testing.T) {
	total, items := summarizeUnread(map[string]int64{"p_1_2": 0, "p_1_3": -1, "p_1_4": 4})
	if total != 4 {
		t.Fatalf("0 与负数不应计入总数，期望 4 实际 %d", total)
	}
	if len(items) != 1 || items[0].ConvID != "p_1_4" {
		t.Fatalf("只应保留 p_1_4，实际 %v", items)
	}

	emptyTotal, emptyItems := summarizeUnread(map[string]int64{})
	if emptyTotal != 0 {
		t.Fatalf("空输入总数应为 0，实际 %d", emptyTotal)
	}
	if emptyItems == nil {
		t.Fatal("空结果必须是非 nil 切片，否则 JSON 会输出 null")
	}
	if len(emptyItems) != 0 {
		t.Fatalf("空输入不应有列表项，实际 %v", emptyItems)
	}
}
