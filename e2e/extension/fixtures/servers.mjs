// E2E 本地服务：fixture 静态页（8899）+ 桌面端 mock 桥（8877）。
// 由 playwright.config.ts 的 webServer 拉起，测试结束自动回收（reuseExistingServer 时可复用）。
// mock 桥协议以 extension/sidepanel/js/net.js 的解析器为唯一事实源。
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const FIXTURES_HTML_DIR = path.resolve(here, '..', 'fixtures-html');
// 真实语料快照（page-corpus）：E2E 冒烟用例经 /corpus/<type>/<page>/source.html 访问
const CORPUS_DIR = path.resolve(here, '..', '..', '..', 'docs', 'page-corpus');
export const STATIC_PORT = 8899;
export const MOCK_PORT = 8877;

// ---------- 工具 ----------

function json(res, obj, status = 200) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(obj));
}

function readBody(req) {
  return new Promise((resolve) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8');
      if (!raw) return resolve(null);
      try { resolve(JSON.parse(raw)); } catch { resolve(raw); }
    });
    req.on('error', () => resolve(null));
  });
}

// ---------- mock 桥（桌面端 HTTP API 替身） ----------

// 最近一次 /api/ask payload：scope-isolation / knowledge 用例用它断言请求归属
export let lastAsk = null;
// 最近一次 /api/knowledge payload；originEventId 已保存集合（模拟服务端幂等去重）
let lastSave = null;
const savedEvents = new Set();
// /api/knowledge 故障注入开关（保存失败提示用例）
let knowledgeFail = false;
// 用例可经 POST /__ask-script 定制 SSE 事件序列（超时/断流/错误码等场景）
let askScript = null;
// 文档卡（PDF 解读 PR3）：最近一次 /api/page/prepare 请求 + 可定制响应
let lastPrepare = null;
let prepareScript = null;

// 事件项可为裸事件对象，或 { delayMs, data }（写该事件前先等待，供停止/断流场景）
async function sse(res, events) {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
  for (const item of events) {
    const { delayMs, data } = item && item.delayMs ? item : { delayMs: 0, data: item };
    if (delayMs) {
      await new Promise((resolve) => {
        const t = setTimeout(resolve, delayMs);
        req2abort(res).then(() => { clearTimeout(t); resolve(); });
      });
    }
    try { res.write(`data: ${JSON.stringify(data)}\n\n`); } catch { return; } // 客户端已断开
  }
  res.end();
}

// 客户端断开（用户点停止 abort）时提前醒来，结束等待
function req2abort(res) {
  return new Promise((resolve) => {
    if (res.writableEnded || res.destroyed) return resolve();
    res.on('close', resolve);
  });
}

function defaultAskScript(payload) {
  return [
    { type: 'session', sessionId: 'mock-session-1' },
    { type: 'token', content: `E2E 回答：收到「${String(payload?.question || '').slice(0, 40)}」` },
    { type: 'token', content: '（mock 后端固定回复）' },
    { done: true, requestId: 'mock-req-1' },
  ];
}

const mock = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${MOCK_PORT}`);
  const body = await readBody(req);
  switch (url.pathname) {
    case '/__last-ask':
      return json(res, { lastAsk });
    case '/__ask-script':
      if (body === null) {
        // 还原默认脚本的同时重置全部 mock 记录态，杜绝跨 spec 文件泄漏
        // （同名 question 会让 pollLastAsk 命中上一个 spec 的陈旧 lastAsk）
        lastAsk = null;
        lastSave = null;
        savedEvents.clear();
        knowledgeFail = false;
        lastPrepare = null;
        prepareScript = null;
      }
      askScript = body;
      return json(res, { ok: true });
    case '/__last-prepare':
      return json(res, { lastPrepare });
    case '/__prepare-script':
      prepareScript = body;
      return json(res, { ok: true });
    case '/api/page/prepare': {
      lastPrepare = body;
      return json(res, prepareScript || {
        documentId: body?.url || '',
        url: body?.url || '',
        finalUrl: body?.url || '',
        title: body?.title || '',
        contentType: 'application/pdf',
        status: 'ready',
        charCount: 28450,
        byteSize: 1024000,
        truncated: false,
        message: ''
      });
    }
    case '/__last-save':
      return json(res, { lastSave });
    case '/__knowledge-fail':
      knowledgeFail = !!body?.on;
      return json(res, { ok: true, knowledgeFail });
    case '/health':
      return json(res, { ok: true });
    case '/api/capabilities':
      return json(res, { imageSupport: false });
    case '/api/cancel':
      return json(res, { ok: true });
    case '/api/ask': {
      lastAsk = body;
      return sse(res, askScript || defaultAskScript(body));
    }
    case '/api/knowledge/draft':
      return json(res, { ok: true, title: 'E2E 草稿标题', summary: 'E2E 草稿摘要' });
    case '/api/knowledge': {
      if (knowledgeFail) {
        return json(res, { error: 'mock bridge: knowledge down' }, 500);
      }
      lastSave = body;
      const dup = !!(body?.originEventId && savedEvents.has(body.originEventId));
      if (body?.originEventId) savedEvents.add(body.originEventId);
      return json(
        res,
        { created: !dup, item: { id: 'mock-kb-1', title: body?.title, aiSummary: body?.aiSummary } },
        dup ? 200 : 201,
      );
    }
    default:
      return json(res, { error: `mock bridge: no route ${req.method} ${url.pathname}` }, 404);
  }
});

// ---------- fixture 静态页 ----------

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
};

const staticSrv = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${STATIC_PORT}`);
  let rel = url.pathname === '/' ? '/index.html' : url.pathname;
  let baseDir = FIXTURES_HTML_DIR;
  if (rel.startsWith('/corpus/')) {
    rel = rel.slice('/corpus'.length);
    baseDir = CORPUS_DIR;
  }
  const file = path.normalize(path.join(baseDir, rel));
  if (!file.startsWith(baseDir)) {
    return json(res, { error: 'forbidden' }, 403);
  }
  try {
    const data = await readFile(file);
    res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
    res.end(data);
  } catch {
    json(res, { error: `not found: ${rel}` }, 404);
  }
});

// ---------- 启动 ----------

mock.listen(MOCK_PORT, '127.0.0.1', () => {
  console.log(`[absorb-e2e] mock bridge  -> http://127.0.0.1:${MOCK_PORT}`);
});
staticSrv.listen(STATIC_PORT, '127.0.0.1', () => {
  console.log(`[absorb-e2e] fixture pages -> http://127.0.0.1:${STATIC_PORT}`);
});
