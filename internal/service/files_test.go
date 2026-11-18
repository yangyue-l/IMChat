package service

import (
	"context"
	"errors"
	"testing"
)

// fakeFiles 记录回收请求，用来验证「哪些对象会被真正删掉」。
type fakeFiles struct {
	calls  int
	keys   []string
	ctxErr error // Delete 拿到的 context 是否已被取消
	err    error
}

func (f *fakeFiles) Delete(ctx context.Context, keys ...string) error {
	f.calls++
	f.keys = append(f.keys, keys...)
	f.ctxErr = ctx.Err()
	return f.err
}

// TestDropObjectsOnlyOwned 锁住回收的边界：只删当前用户上传的对象。
//
// 这几个反例都不是假想：avatar 字段允许用户填任意字符串，把别人的图片地址
// 填进自己的头像后，如果照库里存的值直接删，删掉的就是别人的对象；
// 历史数据与外部图更是连归属都无法判断，只能放过（留孤儿，不影响功能）。
func TestDropObjectsOnlyOwned(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"自己的对象", "u/1001/2026/11/18/1.png", []string{"u/1001/2026/11/18/1.png"}},
		{"别人的对象", "u/1002/2026/11/18/1.png", nil},
		{"历史数据", "2026/11/18/1.png", nil},
		{"默认头像占位符", "avatar://A", nil},
		{"外部图片", "https://img.other.com/a.png", nil},
		{"空值", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := &fakeFiles{}
			dropObjects(context.Background(), files, 1001, c.raw)

			if len(c.want) == 0 {
				if files.calls != 0 {
					t.Fatalf("不该发起删除，实际删了 %v", files.keys)
				}
				return
			}
			if files.calls != 1 || len(files.keys) != 1 || files.keys[0] != c.want[0] {
				t.Fatalf("删除请求 = %v（%d 次），期望 %v", files.keys, files.calls, c.want)
			}
		})
	}
}

// 没有可删的对象时不该发出请求：每次「本来就没有旧图」的操作都多一次
// OSS 往返，纯属浪费配额与延迟。
func TestDropObjectsSkipsEmpty(t *testing.T) {
	files := &fakeFiles{}
	dropObjects(context.Background(), files, 1001)
	dropObjects(context.Background(), files, 1001, "")

	if files.calls != 0 {
		t.Fatalf("空列表不该发起删除，实际 %d 次", files.calls)
	}
}

// 未装配存储（单测、或将来某个不关心回收的部署）时不能 panic。
func TestDropObjectsWithoutStorage(t *testing.T) {
	dropObjects(context.Background(), nil, 1001, "u/1001/2026/11/18/1.png")
}

// 请求 context 已经取消（用户传完图就关页面）时仍然要删，而且不能把
// 已取消的 context 传下去 —— 否则 OSS 调用会立刻失败，孤儿只增不减。
func TestDropObjectsDetachesCanceledContext(t *testing.T) {
	files := &fakeFiles{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dropObjects(ctx, files, 1001, "u/1001/2026/11/18/1.png")

	if files.calls != 1 {
		t.Fatalf("已取消的请求仍应回收，实际调用 %d 次", files.calls)
	}
	if files.ctxErr != nil {
		t.Fatalf("传给存储的 context 不该是已取消状态: %v", files.ctxErr)
	}
}

// 删除失败不能向上抛：调用方已经把业务写成功了，回收只是附带动作。
func TestDropObjectsSwallowsError(t *testing.T) {
	files := &fakeFiles{err: errors.New("oss unreachable")}
	dropObjects(context.Background(), files, 1001, "u/1001/2026/11/18/1.png")
	// 走到这里没 panic 就说明失败被吞住了；删除仍然被尝试过
	if files.calls != 1 {
		t.Fatalf("应当尝试删除一次，实际 %d 次", files.calls)
	}
}
