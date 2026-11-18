package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/config"
	"imchat/internal/middleware"
	"imchat/internal/pkg/snowflake"
	"imchat/internal/storage"
)

func TestMain(m *testing.M) {
	// 上传接口要生成对象 key，而 key 依赖雪花 ID，未初始化会 panic
	if err := snowflake.Init(1); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// fakeStorage 是上传接口的存储替身。
//
// 真跑一次 OSS 需要凭据与网络，而本文件要覆盖的全是「进存储之前」的分支：
// 白名单、大小、空文件、存储报错。body 必须读掉——真实实现会把它读完，
// 不读就测不出「读 body 失败」这条路。
type fakeStorage struct {
	put     []storage.Object
	putErr  error
	delKeys []string
}

func (f *fakeStorage) Put(_ context.Context, obj storage.Object) (string, error) {
	f.put = append(f.put, obj)
	if _, err := io.Copy(io.Discard, obj.Body); err != nil {
		return "", err
	}
	if f.putErr != nil {
		return "", f.putErr
	}
	return f.URL(obj.Key), nil
}

func (f *fakeStorage) URL(key string) string { return "https://cdn.example.com/" + key }

func (f *fakeStorage) Delete(_ context.Context, keys ...string) error {
	f.delKeys = append(f.delKeys, keys...)
	return nil
}

func (f *fakeStorage) Ping(context.Context) error { return nil }

// newUploadRouter 构造只装配了上传依赖的 Router：日志丢给 io.Discard，
// 上限压到 1MB，让「超限」用例不必真的造一个 10MB 的文件。
// Timeout 要显式给：真实配置走 applyDefaults，手写的 cfg 不会。
func newUploadRouter(files storage.Storage) *Router {
	return &Router{
		cfg: &config.Config{Upload: config.UploadConfig{
			MaxSizeMB: 1,
			Timeout:   config.Duration(120 * time.Second),
		}},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		files: files,
	}
}

// callUpload 以 uid 身份调用上传接口，返回状态码与解析后的响应。
func callUpload(t *testing.T, r *Router, uid int64, filename string, content []byte) (int, uploadBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写入文件内容失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/upload", &buf)
	c.Request.Header.Set("Content-Type", w.FormDataContentType())
	// 真实链路上这个值由 middleware.Auth 写入，单测直接塞进去
	c.Set(middleware.UIDKey, uid)

	r.upload(c)

	var body uploadBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v，原文 %s", err, rec.Body.String())
	}
	return rec.Code, body
}

// uploadBody 是上传接口的响应形状，只声明本文件要断言的字段。
type uploadBody struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Key  string `json:"key"`
		URL  string `json:"url"`
		Size int64  `json:"size"`
	} `json:"data"`
}

func TestUploadRejectsInvalidInput(t *testing.T) {
	limit := int64(1) << 20
	cases := []struct {
		name     string
		filename string
		content  []byte
		wantMsg  string
	}{
		// 白名单按扩展名挡可执行文件；后缀大小写不该影响判定
		{"不支持的格式", "a.exe", []byte("MZ"), "仅支持"},
		{"无扩展名", "README", []byte("x"), "仅支持"},
		{"空文件", "a.png", nil, "文件内容为空"},
		{"超过上限", "a.png", bytes.Repeat([]byte("x"), int(limit)+1), "不能超过 1MB"},
		// 连报文余量都超了，会被 MaxBytesReader 拦下；提示必须与普通超限一致，
		// 不能退化成「请选择要上传的文件」
		{"请求体超过硬上限", "a.png", bytes.Repeat([]byte("x"), int(limit+multipartOverhead)+1), "不能超过 1MB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStorage{}
			code, body := callUpload(t, newUploadRouter(st), 1001, tc.filename, tc.content)

			if code != http.StatusBadRequest || body.Code != 40000 {
				t.Fatalf("状态码 = %d/%d，期望 400/40000（msg=%s）", code, body.Code, body.Msg)
			}
			if !strings.Contains(body.Msg, tc.wantMsg) {
				t.Fatalf("提示文案未点明原因: %q", body.Msg)
			}
			// 校验没过的请求不该写进存储，否则桶里全是无主垃圾
			if len(st.put) != 0 {
				t.Fatalf("校验失败却调用了 Put: %+v", st.put)
			}
		})
	}
}

// 正好等于上限的文件必须放行：MaxBytesReader 限制的是整个 multipart 请求体，
// 上限没给报文头留余量的话，10MB 的文件会因为多出几百字节被拒。
func TestUploadAcceptsFileAtLimit(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 1<<20)
	st := &fakeStorage{}
	code, body := callUpload(t, newUploadRouter(st), 1001, "a.PNG", content)

	if code != http.StatusOK || body.Code != 0 {
		t.Fatalf("状态码 = %d/%d，期望 200/0（msg=%s）", code, body.Code, body.Msg)
	}
	if len(st.put) != 1 {
		t.Fatalf("Put 调用次数 = %d，期望 1", len(st.put))
	}
	// 后缀统一小写后再落 Content-Type，否则 .PNG 会落到 octet-stream
	if got := st.put[0].ContentType; got != "image/png" {
		t.Fatalf("ContentType = %q，期望 image/png", got)
	}
	if st.put[0].Size != int64(len(content)) {
		t.Fatalf("Size = %d，期望 %d", st.put[0].Size, len(content))
	}
}

func TestUploadReturnsKeyAndURL(t *testing.T) {
	st := &fakeStorage{}
	code, body := callUpload(t, newUploadRouter(st), 1001, "a.webp", []byte("img"))

	if code != http.StatusOK || body.Code != 0 {
		t.Fatalf("状态码 = %d/%d，期望 200/0（msg=%s）", code, body.Code, body.Msg)
	}
	// key 必须以上传者 uid 开头：回收对象前靠这一段判断归属
	if !strings.HasPrefix(body.Data.Key, "u/1001/") {
		t.Fatalf("key = %q，期望以 u/1001/ 开头", body.Data.Key)
	}
	if body.Data.Key != st.put[0].Key {
		t.Fatalf("响应里的 key 与实际写入的 key 不一致：%q vs %q", body.Data.Key, st.put[0].Key)
	}
	if body.Data.URL != st.URL(body.Data.Key) {
		t.Fatalf("url = %q，与 key 推导出的地址不一致", body.Data.URL)
	}
	if body.Data.Size != 3 {
		t.Fatalf("size = %d，期望 3", body.Data.Size)
	}
}

// 存储失败只能回 500：接口不向客户端泄露桶名、凭据状态这类内部信息，
// 具体原因留在日志里。
func TestUploadReportsStorageFailure(t *testing.T) {
	st := &fakeStorage{putErr: storage.ErrUnavailable}
	code, body := callUpload(t, newUploadRouter(st), 1001, "a.png", []byte("img"))

	if code != http.StatusInternalServerError || body.Code != 50000 {
		t.Fatalf("状态码 = %d/%d，期望 500/50000", code, body.Code)
	}
	if strings.Contains(body.Msg, "storage") || strings.Contains(body.Msg, "oss") {
		t.Fatalf("响应泄露了存储细节: %q", body.Msg)
	}
}

// 上传指标只数「真正进了存储层」的那一步。
//
// 参数校验失败混进来会让失败率在用户乱传文件时飙升，把真正的存储故障淹掉；
// 成功数与字节数则要能对上，否则容量规划没有依据。
func TestUploadMetrics(t *testing.T) {
	r := newUploadRouter(&fakeStorage{})

	callUpload(t, r, 1001, "a.png", []byte("12345")) // 成功，5 字节
	callUpload(t, r, 1001, "a.exe", []byte("x"))     // 白名单拒绝，不计入
	callUpload(t, r, 1001, "a.png", []byte("123"))   // 成功，3 字节

	if got := r.uploadOK.Load(); got != 2 {
		t.Fatalf("成功计数 = %d，期望 2", got)
	}
	if got := r.uploadFailed.Load(); got != 0 {
		t.Fatalf("参数校验失败不应计入存储失败，实际 %d", got)
	}
	if got := r.uploadBytes.Load(); got != 8 {
		t.Fatalf("字节计数 = %d，期望 8", got)
	}

	// 存储报错才计入失败
	r.files = &fakeStorage{putErr: storage.ErrUnavailable}
	callUpload(t, r, 1001, "a.png", []byte("x"))
	if got := r.uploadFailed.Load(); got != 1 {
		t.Fatalf("存储失败计数 = %d，期望 1", got)
	}
	if got := r.uploadOK.Load(); got != 2 {
		t.Fatalf("存储失败不该计入成功，实际成功数 %d", got)
	}
}

// delayedReader 在第一次 Read 时先睡一段时间，把请求体拖慢。
// 用「睡一次」而不是「每次读都睡」：总耗时与客户端的读次数无关，用例才稳定。
type delayedReader struct {
	r     io.Reader
	delay time.Duration
	once  sync.Once
}

func (d *delayedReader) Read(p []byte) (int, error) {
	d.once.Do(func() { time.Sleep(d.delay) })
	return d.r.Read(p)
}

// 上传必须自己重设连接级 read deadline。
//
// http.Server 的 ReadTimeout 覆盖「读整个请求体」，15s 对 10MB 的弱网上传是不够的；
// 而请求级 context 超时管不到连接 —— 时间一到标准库直接关连接，后端连一句错误都发不出去。
// 这里把 ReadTimeout 压到 200ms、把请求体拖到 600ms：只有 handler 里那句
// ResponseController.SetReadDeadline 生效，请求才可能成功。
func TestUploadExtendsConnReadDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	st := &fakeStorage{}
	r := newUploadRouter(st)
	r.cfg.Upload.Timeout = config.Duration(3 * time.Second)

	e := gin.New()
	e.MaxMultipartMemory = 1 << 20
	e.POST("/upload", func(c *gin.Context) {
		c.Set(middleware.UIDKey, int64(1001))
		r.upload(c)
	})

	srv := httptest.NewUnstartedServer(e)
	srv.Config.ReadTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", "a.png")
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err := part.Write([]byte("img")); err != nil {
		t.Fatalf("写入文件内容失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}

	// 用 io.Reader 而不是 *bytes.Buffer，让 transport 走分块传输，
	// 这样 delayedReader 的第一次 Read 才真的发生在发 body 的时候。
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/upload",
		&delayedReader{r: bytes.NewReader(buf.Bytes()), delay: 600 * time.Millisecond})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("慢速上传应当成功（连接 deadline 未被延长？）: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200，响应 %s", resp.StatusCode, body)
	}
	if len(st.put) != 1 {
		t.Fatalf("应当写入一次对象，实际 %d 次", len(st.put))
	}
}
