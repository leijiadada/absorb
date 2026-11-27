module absorb-agent

go 1.27.4

// 评测链路专用：evaluation v1.11.0 自身 pin 的 model/* 子模块 v0.8.0 与核心 v1.11.2
// 不兼容（缺 imodel.DefaultOutputTokensFloor），此处显式升到与核心同代的 v1.11.0（MVS 取高者）。
require (
	github.com/energye/systray v1.0.3
	github.com/go-ego/gse v1.0.0
	github.com/google/uuid v1.6.0
	github.com/openai/openai-go v1.12.0
	github.com/unitedrhino/docling v1.6.3
	github.com/wailsapp/wails/v2 v2.16.0
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.29.0
	go.opentelemetry.io/otel/sdk v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
	go.uber.org/goleak v1.3.0
	go.uber.org/zap v1.27.0
	golang.org/x/image v0.48.0
	golang.org/x/net v0.62.0
	golang.org/x/sys v0.50.0
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.29.0
	trpc.group/trpc-go/trpc-agent-go v1.11.2
	// M0 评测（cmd/absorb-eval）：evaluation 是框架独立子模块，最新发布 v1.11.0
	trpc.group/trpc-go/trpc-agent-go/evaluation v1.11.0
	trpc.group/trpc-go/trpc-agent-go/memory/sqlite v1.11.0
	trpc.group/trpc-go/trpc-agent-go/tool/webfetch/httpfetch v1.11.0
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.18.2 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	git.sr.ht/~jackmordaunt/go-toast/v2 v2.0.3 // indirect
	github.com/JohannesKaufmann/dom v0.2.0 // indirect
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.4.0 // indirect
	github.com/anthropics/anthropic-sdk-go v1.37.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bep/debounce v1.2.1 // indirect
	github.com/bmatcuk/doublestar/v4 v4.9.1 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/bytexlee/doc v0.0.0-20260702044225-e57d9125d392 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/creack/pty v1.1.24 // indirect
	github.com/dkrisman/gobig2 v0.0.0-20260513123937-51e39052fde6 // indirect
	github.com/dslipak/pdf v0.0.2 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.11 // indirect
	github.com/googleapis/gax-go/v2 v2.17.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/hhrutter/tiff v1.0.6 // indirect
	github.com/jchv/go-winloader v0.0.0-20210711035445-715c2860da7e // indirect
	github.com/labstack/echo/v4 v4.13.3 // indirect
	github.com/labstack/gommon v0.4.2 // indirect
	github.com/leaanthony/go-ansi-parser v1.6.1 // indirect
	github.com/leaanthony/gosod v1.0.4 // indirect
	github.com/leaanthony/slicer v1.6.0 // indirect
	github.com/leaanthony/u v1.1.1 // indirect
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728 // indirect
	github.com/mailru/easyjson v0.9.0 // indirect
	github.com/mattetti/filebuffer v1.0.1 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/mrjoshuak/go-jpeg2000 v1.5.12 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/neurosnap/sentences v1.1.2 // indirect
	github.com/nkiri/xls v0.0.4 // indirect
	github.com/ollama/ollama v0.17.1 // indirect
	github.com/panjf2000/ants/v2 v2.10.0 // indirect
	github.com/pdfcpu/pdfcpu v0.15.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/qmuntal/gltf v0.29.0 // indirect
	github.com/raitucarp/epub v0.5.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/richardlehane/mscfb v1.0.7 // indirect
	github.com/richardlehane/msoleps v1.0.6 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/samber/lo v1.49.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/tiendc/go-deepcopy v1.7.2 // indirect
	github.com/tkrajina/go-reflector v0.5.8 // indirect
	github.com/unitedrhino/go-cad v0.0.0-20261001090957-ddd91eea9618 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	github.com/vcaesar/cedar v0.20.2 // indirect
	github.com/wailsapp/go-webview2 v1.0.22 // indirect
	github.com/wailsapp/mimetype v1.4.1 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/xuri/efp v0.0.1 // indirect
	github.com/xuri/excelize/v2 v2.11.0 // indirect
	github.com/xuri/nfp v0.0.2-0.20250530014748-2ddeb826f9a9 // indirect
	github.com/yuin/goldmark v1.7.13 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.63.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.45.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.45.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/term v0.48.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/tools v0.52.1-0.20261106145633-d407941a339a // indirect
	google.golang.org/genai v1.36.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.41.0 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.7.2 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
	trpc.group/trpc-go/trpc-a2a-go v0.2.6-0.20260721084546-18c8244d0acb // indirect
	trpc.group/trpc-go/trpc-agent-go/model/anthropic v1.11.0 // indirect
	trpc.group/trpc-go/trpc-agent-go/model/gemini v1.11.0 // indirect
	trpc.group/trpc-go/trpc-agent-go/model/ollama v1.11.0 // indirect
	trpc.group/trpc-go/trpc-agent-go/model/provider v1.11.0 // indirect
)

//// 本地补丁：修复 Windows 上 CollectOutputs/Collect 的 glob 匹配失败
//// （ds.Glob(os.DirFS("/"), 反斜杠绝对路径) 在 Windows 上永远匹配不到）。
//// 补丁目录含完整 v1.11.2 源码 + workspace_runtime.go 修复，可独立编译。
//replace trpc.group/trpc-go/trpc-agent-go => ../../trpc-agent-go-v1.11.2

// 长期记忆：sqlite 后端是框架独立子模块（v1.11.2 尚未发布该子模块），
// replace 指向 main 快照源码目录；其内部依赖的 trpc-agent-go 经上一条 replace 统一解析到 v1.11.2 本地补丁版。
//require trpc.group/trpc-go/trpc-agent-go/memory/sqlite v1.11.2

//replace trpc.group/trpc-go/trpc-agent-go/memory/sqlite => ../../trpc-agent-go-main/memory/sqlite
