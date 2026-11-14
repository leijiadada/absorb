# Absorb 阅读助手浏览器插件 — 设计系统规范

> 适用项目：`extension/`（Chrome 扩展 Side Panel）
> 基于现有 `sidepanel.css` 的 14 个 CSS 变量、`sidepanel.html` 的 5 大模块、`sidepanel.js` 的交互约定整合
> 最近更新：2026-08-30

---

## 1. 设计原则（基线）

| 原则 | 说明 |
|---|---|
| **IDE 风格对话流** | 助手回复为无背景无边框的文本流（左对齐于容器右边缘），用户消息为右对齐气泡；两者整体右对齐，区别于传统左右对话气泡布局 |
| **少即是多** | 一屏内不超过 2 个主色（蓝 + 红），其余走中性灰阶 |
| **4px 栅格** | 间距以 4 的倍数为基准（4/8/12/16/24），允许 2/6/10 等次级档位适配紧凑面板 |
| **窄面板优先** | Side Panel 宽度由浏览器决定（约 320–500px），所有组件必须可在 320px 宽度下不溢出 |
| **状态可感** | loading（流式 caret）/ empty（empty-tip）/ error（红字 + 提示）三态必须有 |
| **防误触** | 危险操作（清空历史）必须有二次确认；点击外部关闭抽屉而非直接清空 |
| **避免闪烁** | 不因网络延迟产生 UI 突变；流式增量只追加，不重建；切换前确保目标态就绪 |
| **智能粘底** | 流式输出仅当用户在底部附近（≤ 80px 阈值）才自动滚动，避免打断阅读历史 |

---

## 2. 色板（color system）

> 定义位置：`sidepanel.css` 的 `:root` 块
> 命名规则：语义化短名（`--bg` / `--primary` 等），禁止使用色值字面量

### 2.1 主色板

| 变量 | 色值 | 用途 | 禁用 |
|---|---|---|---|
| `--bg` | `#f7f8fa` | 页面背景、按钮 hover 底、输入框底 | ❌ 用作卡片背景（与 surface 对比太弱） |
| `--surface` | `#ffffff` | 卡片/抽屉/弹窗背景 | — |
| `--primary` | `#2f6fd4` | 品牌主色（用户气泡、主按钮、流式光标、focus 边框、链接强调） | ❌ 用作正文色（对比度不足） |
| `--primary-hover` | `#2a61bd` | 主色按钮 hover | — |
| `--primary-soft` | `#eaf0fb` | 浅蓝（历史项激活态背景、信息块） | ❌ 用作正文背景 |
| `--danger` | `#d9544f` | 危险色（清空按钮文字、错误提示） | ❌ 与主色同屏混用 |
| `--danger-soft` | `#fbeceb` | 浅红（危险按钮 hover 底） | — |

### 2.2 中性灰阶

| 变量 | 色值 | 用途 |
|---|---|---|
| `--text` | `#1f2329` | 强文字（正文、标题） |
| `--text-2` | `#4b5159` | 次要文字（ghost 按钮文字、field-label） |
| `--muted` | `#8a9099` | 弱文字（subtitle、hint、role、meta、empty 提示） |
| `--border` | `#e3e6eb` | 默认边框、分隔线 |
| `--border-strong` | `#cdd2d9` | hover 边框、空状态插画色 |

### 2.3 派生色（未定义为变量但代码中固定）

| 色值 | 用途 | 备注 |
|---|---|---|
| `#fff` | 用户气泡文字、主按钮文字、ghost 按钮背景 | — |
| `#b9c4d8` | 主按钮禁用态背景 | 与主色同色系降饱和 |
| `#d8e3f6` | 历史项激活态边框 | 与 primary-soft 配对 |
| `#f0c4c2` | 危险按钮 hover 边框 | 与 danger-soft 配对 |
| `#d4d7dd` / `#b9bec6` | 滚动条 thumb 默认/hover | — |
| `rgba(0,0,0,.04) / .18 / .35` | 阴影、settings 遮罩 | — |

### 2.4 语义色（业务约定）

> **本项目核心约定：用户消息 = 蓝气泡，助手消息 = 无装饰文本流**
> 不引入额外状态色（成功/警告），错误一律用 `--danger` 红字 + 文字提示

| 语义 | 色值 | 备注 |
|---|---|---|
| 用户消息 | `--primary` + `#fff` | 蓝底白字气泡，右对齐 |
| 助手消息 | `--text` + 透明 | 黑字无背景，右贴边文本流 |
| 错误提示 | `--danger` 文字 | 前缀 `⚠️`，无背景，附排查建议 |
| 流式进行中 | `--primary` caret | 闪烁光标 + assistant.streaming 类 |

**对比度要求**：正文 vs 背景 ≥ 4.5:1（`#1f2329` vs `#ffffff` ≈ 16:1 ✓）

---

## 3. 字号阶梯（type scale）

> Chrome Side Panel 走桌面 px 单位，body 默认 14px

| Token | 字号 | 字重 | 行高 | 用途 |
|---|---|---|---|---|
| `display` | 16px | 700 | 1 | Logo "A" |
| `h1` | 15px | 600 | 1.15 | Brand 标题 "Absorb" |
| `h2` | 14px | 600 | 1.4 | 抽屉/设置 header 标题（继承 body） |
| `body` | 14px | 400 | 1.5 | 默认正文 |
| `body-md` | 13px | 400 | 1.6 | 气泡、context-text、empty-tip、输入框、按钮文字 |
| `body-sm` | 13px | 400 | 1.55 | context-text 行高（max-height 130px 内滚动） |
| `caption` | 12px | 400 | 1.4 | icon-btn、field-label、empty-tip.muted、history-empty |
| `micro` | 11px | 400 / 500 | 1.4 | subtitle、hint、ctx-source、box-label、role、h-meta、field-hint |

**约束**：
- 字体栈：`-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Helvetica Neue", Arial, sans-serif`
- 不允许出现字号 < 11px（影响可读性）
- 不允许出现字号 > 16px（破坏窄面板信息密度）
- 按钮文字最小 12px

---

## 4. 间距系统（spacing）

> 4px 栅格统一，常用档位：

| Token | 值 | 用途 |
|---|---|---|
| `space-1` | 4px | 极小间距（msg.role 与 bubble 之间 gap、role 左右内 padding 2px） |
| `space-2` | 6px | header-actions / drawer-actions 按钮间 gap、history-list padding |
| `space-3` | 8px | 输入框/气泡内 padding、history-list li padding、messages gap 12px 起点 |
| `space-4` | 10px | icon-btn padding（5px 10px）、header/composer 上下 padding |
| `space-5` | 12px | main padding（左右）、messages gap、empty-tip 上下 padding |
| `space-6` | 14px | header/composer/main 左右 padding、settings-body padding |
| `space-8` | 16px | messages padding-bottom、settings-overlay padding |
| `space-12` | 24px | empty-tip 上下 padding、history-empty 上下 padding |
| `space-14` | 28px | Logo 尺寸 28×28 |

**模块外边距约定**：
- header / composer / main 左右内 padding：14px
- 区块间垂直 gap：10px（main 内 context-box 与 messages 间）
- messages 自身上下 padding-bottom：16px（为最后一条消息留呼吸感）

---

## 5. 圆角（radius）

| 名称 | 值 | 用途 |
|---|---|---|
| `--radius` | 8px | 默认圆角变量（icon-btn / primary-btn / ghost-btn / context-box / history-list li / field input） |
| `radius-sm` | 4px | 滚动条 thumb、流式光标圆角（隐式） |
| `radius-lg` | 10px | settings-card |
| `radius-bubble` | `12px 12px 2px 12px` | 用户气泡（右上尖角指向自己） |
| `radius-none` | 0 | 助手气泡（IDE 文本流，无装饰） |
| `radius-pill` | 50% | Logo 方块 8px 圆角（非 pill，但同尺寸约定） |

**约束**：圆角不超过 `min(高, 宽) / 2`；助手气泡禁止任何圆角/边框/背景

---

## 6. 阴影（elevation）

| 名称 | 值 | 用途 |
|---|---|---|
| `--shadow` | `0 1px 2px rgba(0,0,0,.04), 0 2px 8px rgba(0,0,0,.04)` | history-drawer（侧抽屉浮起） |
| `shadow-modal` | `0 8px 28px rgba(0,0,0,.18)` | settings-card（模态浮起） |
| `shadow-none` | none | 列表项、消息气泡（避免视觉噪声） |

**约定**：
- 阴影用低透明度黑（≤ 0.18），禁用纯黑 0.5+
- 抽屉类用双层叠加 shadow 增加层次感
- 模态用单层大模糊营造空间感

---

## 7. 动效规范（animation）

### 7.1 缓动与时长

| 场景 | 时长 | 属性 | 备注 |
|---|---|---|---|
| 按钮点击反馈 | 0.15s | `background-color, border-color, color` | 所有 button 默认 transition |
| 输入框 focus | 0.15s | `border-color` | input 默认 transition |
| 流式光标闪烁 | 1s | `opacity` | `@keyframes blink` steps(2, start) infinite |
| 抽屉显隐 | 即时 | `hidden` 属性切换 | 不做过渡（避免窄面板抖动） |
| 设置弹窗显隐 | 即时 | `hidden` 属性切换 | 同上 |

### 7.2 流式光标实现

```css
.stream-caret {
  display: inline-block;
  width: 7px;
  height: 14px;
  background: var(--primary);
  margin-left: 2px;
  vertical-align: text-bottom;
  animation: blink 1s steps(2, start) infinite;
}
@keyframes blink { 0%, 50% { opacity: 1; } 50.01%, 100% { opacity: 0; } }
```

**约束**：
- 流式期间 assistant 消息加 `.streaming` 类，结束移除
- 光标始终追加在 bubble 末尾（token 增量时 `bubble.appendChild(caret)` 保持位置）
- 流式结束后 `caret.remove()`，不允许残留

### 7.3 滚动行为

| 场景 | 行为 |
|---|---|
| 新消息进入 | 智能粘底：`isNearBottom()` 返回 true 时 `scrollToBottom()`，否则不打断 |
| 流式开始时 | 锁定 `stickToBottom = isNearBottom()`，全程跟随该初值 |
| 用户主动滚上 | 不被强制拉回 |
| 历史切换 | `renderMessages()` 后无条件 `scrollToBottom()` |

**阈值**：`SNAP_THRESHOLD = 80`（px），距底 ≤ 80px 视为"在底部"

---

## 8. 组件层级（z-index）

| 层级 | 值 | 用途 |
|---|---|---|
| `z-base` | 0 | 默认 |
| `z-drawer` | 10 | history-drawer |
| `z-modal` | 50 | settings-overlay |
| `z-toast` | — | 暂无（用 alert/confirm 原生） |

**约束**：抽屉与模态不同时显示（新对话时强制收起抽屉），避免 z-index 叠加歧义

---

## 9. 命名约定（naming）

### 9.1 CSS 类名

> 采用「语义短名 + BEM 简化」混合策略，与现有代码一致

```
.block              块      .app-header / .context-box / .messages / .composer / .history-drawer
.block-element      元素    .brand-text / .drawer-header / .drawer-actions / .box-label / .composer-actions
.block--modifier    修饰符  .ghost-btn.danger / .context-text.empty
.block.is-state     状态    .msg.user / .msg.assistant.streaming / .history-list li.active
```

**id 命名**：camelCase，挂在 JS 需要操作的元素上（`#questionInput` / `#goServerUrl` / `#historyList` 等）

**禁止**：
- ❌ 标签选择器裸用（除非 reset）
- ❌ `!important`（`[hidden] { display: none !important; }` 是唯一例外，防止 flex 覆盖 hidden）
- ❌ 行内 style（动态计算值除外）

### 9.2 JS 命名

| 类型 | 命名 | 示例 |
|---|---|---|
| DOM 引用对象 | `els` | `els.historyDrawer` |
| 状态对象 | `state` | `state.sessions / state.currentSessionId` |
| 布尔 | `is/has` | `isStreaming / stickToBottom` |
| 常量 | `UPPER_SNAKE` | `STORAGE_KEY / SETTINGS_KEY / SNAP_THRESHOLD` |
| 渲染函数 | `renderXxx` | `renderContext / renderMessages / renderHistoryList` |
| 异步函数 | `async function` + 动词 | `loadState / saveState / callAgent / submitQuestion` |
| 事件处理 | `onXxx` 或直接挂监听 | 优先内联 `els.xxx.addEventListener` |

---

## 10. 状态规范（三态 + 多态）

### 10.1 消息三态

| 状态 | 必备元素 | 实现 |
|---|---|---|
| **empty** | 居中插画 `❝` + 灰色提示 + 次要说明 | `.empty-tip`，`display: ''` 显式恢复 |
| **list** | 消息气泡流 | `.msg` 节点追加到 `#messages` |
| **streaming** | 末尾追加 `assistant.streaming` 气泡 + `.stream-caret` | `buildMsgNode(role, '', true)` |
| **error** | 气泡内容前缀 `⚠️` + 错误信息 + 排查建议 | 直接改 `assistantMsg.content` |

### 10.2 流式态关键节点

| 事件 | UI 行为 |
|---|---|
| 用户提交问题 | 追加 user 气泡，清空输入框，`isStreaming = true`，禁用按钮 |
| 收到首个 token | 追加空 assistant 气泡 + streaming 类 + caret |
| token 持续到达 | `bubble.textContent = ''` + `appendChild(textNode)` 重建（保证 caret 位置） |
| 收到 `done:true` | 移除 streaming 类 + caret，回填 sessionId |
| 收到 `error` 事件 | 气泡变红字错误提示 |
| 网络异常 | catch 写入 `⚠️ 请求失败` + 排查建议 |
| 流式结束（finally） | `isStreaming = false`，启用按钮，尝试消费 pendingSelection |

### 10.3 抽屉/弹窗态

| 状态 | 实现 |
|---|---|
| 历史抽屉打开 | `historyDrawer.hidden = false` + `renderHistoryList()` |
| 历史抽屉关闭 | `historyDrawer.hidden = true`（点 × / 点面板外部 / 新对话时触发） |
| 设置弹窗打开 | `settingsOverlay.hidden = false` + 清空 connResult + 聚焦输入框 |
| 设置弹窗关闭 | `settingsOverlay.hidden = true`（点 × / 保存后触发） |

### 10.4 输入态

| 场景 | UI |
|---|---|
| 输入框空 | 提问按钮 disabled，背景灰 `#b9c4d8` |
| 输入有内容 | 提问按钮 enabled，背景 `--primary` |
| 流式中 | 提问按钮 disabled（不论输入） |
| 流式中右键划词 | 暂存到 `pendingSelection`，流式结束后 50ms 重试消费 |

---

## 11. 可访问性（A11Y）

### 11.1 必备

- [ ] 所有 icon-btn 给 `aria-label`（"设置" / "查看历史" / "新对话" / "关闭历史" / "关闭设置"）
- [ ] 所有 icon-btn 给 `title`（hover tooltip）
- [ ] 消息流容器 `aria-live="polite"`（流式输出对屏幕阅读器友好）
- [ ] 文字 vs 背景对比度 ≥ 4.5:1
- [ ] 按钮文字最小 12px
- [ ] 危险操作二次确认（`confirm()`）

### 11.2 焦点

- 输入框 focus 时 `border-color: var(--primary)` + `background: #fff`（视觉反馈明显）
- 弹窗打开后 `input.focus()` 自动聚焦主输入
- 新对话后 `questionInput.focus()`
- 流式结束后不抢焦点（用户可能在选词）

### 11.3 颜色不冗余

- 用户/助手区别靠「气泡 vs 无装饰」+ 「右对齐 vs 右贴边」，不靠颜色
- 流式态靠 caret 闪烁 + `.streaming` 边框提示，不靠颜色

---

## 12. 性能基线

| 指标 | 目标 |
|---|---|
| 首屏渲染 | ≤ 200ms（纯本地，无网络） |
| SSE 首 token | ≤ 1s（含网络 + 模型推理） |
| 消息滚动 | 60fps（流式增量只改 textContent，不重建结构） |
| 历史列表渲染 | ≤ 50ms（最多 50 条会话） |
| chrome.storage 读写 | 异步不阻塞 UI |

**优化手段**：
- 流式增量用 `bubble.textContent = ''` + `appendChild(textNode)` 重建，避免 innerHTML 解析开销
- 历史列表全量重渲染（数据量 ≤ 50 条，性能可接受），避免 diff 复杂度
- `consumePendingSelection` 流式期间短路（`if (isStreaming) return false`）
- 流式结束 50ms 后再消费 pendingSelection，避免立即触发新一轮 UI 抖动

---

## 13. 组件清单

### 13.1 布局组件

| 组件 | class | 关键约束 |
|---|---|---|
| Header | `.app-header` | flex row, `padding: 10px 14px`, border-bottom, `flex-shrink: 0` |
| Main | `.main` | flex column, `padding: 12px 14px`, `min-height: 0`, gap 10px |
| Composer | `.composer` | flex-shrink: 0, border-top, `padding: 10px 14px` |
| Body 容器 | `body` | flex column, `height: 100vh`, `overflow: hidden` |

### 13.2 业务组件

| 组件 | class | 关键约束 |
|---|---|---|
| Logo | `.logo` | 28×28, radius 8px, bg primary, white, 700/16px, 居中 |
| Brand | `.brand` + `.brand-text` | flex gap 10px，h1 15px/600，subtitle 11px/muted |
| Icon Button | `.icon-btn` | transparent + border, padding 5px 10px, font 12px |
| Context Box | `.context-box` | bg var(--bg), border, radius, padding 8px 10px, flex-shrink 0 |
| Messages | `.messages` | flex column, gap 12px, `padding-bottom: 16px`, overflow-y auto, flex 1 |
| Empty Tip | `.empty-tip` | margin auto, 13px muted, 居中插画 + 双行提示 |
| Message | `.msg` | flex column, gap 4px, `.role` 11px muted + `.bubble` 13px |
| User Bubble | `.msg.user .bubble` | bg primary, white, radius `12px 12px 2px 12px`, max-width 85%, align-items flex-end |
| Assistant Bubble | `.msg.assistant .bubble` | transparent, no border, radius 0, max-width 100%, padding 4px 2px |
| Stream Caret | `.stream-caret` | 7×14, bg primary, blink 1s |
| Question Input | `#questionInput` | width 100%, resize none, padding 8px 10px, font 13px, bg var(--bg) |
| Primary Button | `.primary-btn` | bg primary, white, padding 7px 18px, 600 |
| Ghost Button | `.ghost-btn` | bg #fff, border, color text-2, padding 4px 10px |
| History Drawer | `.history-drawer` | absolute right, width 80% max 320px, z-index 10 |
| History Item | `.history-list li` | padding 8px 10px, radius 8px, hover bg, active primary-soft |
| Settings Overlay | `.settings-overlay` | fixed inset 0, bg rgba(0,0,0,.35), flex center, z-index 50 |
| Settings Card | `.settings-card` | bg surface, radius 10px, max-width 340px |

### 13.3 关键全局规则

```css
/* 防止 flex 覆盖 hidden 属性（历史抽屉曾因此关不掉） */
[hidden] { display: none !important; }

/* 全局盒模型 */
* { box-sizing: border-box; }
html, body { height: 100%; margin: 0; }
```

---

## 14. 交互规范

### 14.1 提问流程

1. 用户在 `#questionInput` 输入，Enter（无 Shift）或点「提问」
2. `submitQuestion()` → 取当前会话（无则新建）→ push user message → render → `streamAssistantReply()`
3. 流式接收 token，追加到当前 assistant 气泡
4. 收到 `done:true` 回填 sessionId（用于多轮上下文）
5. 结束后 `saveState()` 持久化，尝试消费 pendingSelection

### 14.2 划词流程

| 入口 | 行为 |
|---|---|
| 网页右键划词 → 菜单 → sidepanel 打开 | background 写 `pendingSelection` 到 chrome.storage，sidepanel init 时 `consumePendingSelection` |
| 面板已开 + 再次右键划词 | `chrome.storage.onChanged` 监听 → 流式中暂存，结束后 50ms 消费 |
| 流式中划词 | `consumePendingSelection` 立即短路返回 false，避免打断当前回答 |

### 14.3 历史抽屉交互

| 触发 | 行为 |
|---|---|
| 点「历史」按钮 | toggle 抽屉显隐，显时 `renderHistoryList()` |
| 点抽屉内列表项 | 切换 currentSessionId，重渲染 context/messages/historyList |
| 点「清空」 | `confirm()` 二次确认 → 清空 sessions → 重渲染 |
| 点「×」 | 关闭抽屉 |
| 点抽屉外部（面板其他区域） | 关闭抽屉（`stopPropagation` 防误伤 toggle 按钮） |
| 点「新对话」 | 关闭抽屉 + currentSessionId = null + 清空 context/messages + 聚焦输入框 |
| 点「设置」 | 关闭抽屉（隐式，document click 监听）+ 打开设置弹窗 |

### 14.4 SSE 协议事件

| 事件 | 处理 |
|---|---|
| `data: {"type":"token","content":"..."}` | `onToken(content)` 追加到当前气泡 |
| `data: {"type":"session","sessionId":"..."}` | 回填 `gotSessionId`，不渲染 |
| `data: {"done":true,...}` | return，结束流式 |
| `data: {"type":"error","message":"..."}` | throw，进入 catch 写错误气泡 |
| HTTP 非 2xx | throw `服务返回 HTTP {status}` |

---

## 15. 禁忌清单

- ❌ 给助手气泡加背景/边框/圆角（破坏 IDE 文本流风格）
- ❌ 用户气泡左对齐（必须右对齐到容器右边缘）
- ❌ 字号 < 11px
- ❌ 圆角超过元素一半
- ❌ 同色系作文字 vs 背景
- ❌ 阴影用纯黑 `rgba(0,0,0,0.5+)`（视觉过重）
- ❌ 一屏超过 2 个主色（蓝 + 红已是上限）
- ❌ 按钮禁用态不透明度 < 0.5（视觉残留）
- ❌ 流式期间重建整个消息容器（性能 + 焦点丢失）
- ❌ 历史抽屉用过渡动画（窄面板抖动）
- ❌ 删除 `[hidden] { display: none !important; }`（flex 会覆盖 hidden 导致抽屉关不掉）
- ❌ 流式 token 用 `innerHTML` 累加（XSS + 性能）
- ❌ 同一变量在 CSS 与 JS 中重复硬编码色值
- ❌ 不做二次确认的危险操作
- ❌ 流式中未禁用「提问」按钮（用户中断当前流式）

---

## 16. 文档维护

- 任何新增色值/字号/间距/组件，必须先在此文档登记
- 设计变更需 PR 同步更新本文件
- 代码 review 时 check 本文件 checklist
- 新增组件必须列入 §13 组件清单
- 修改 §10 状态流转必须同步更新 sidepanel.js 注释

---

## 附录：Side Panel 布局示意

```
┌──────────────────────────────────────────────┐
│ [A] Absorb              ⚙  历史  ＋新          │ ← app-header (flex-shrink 0)
├──────────────────────────────────────────────┤
│ 当前句子                         网页来源      │
│ ┌────────────────────────────────────────┐  │
│ │ "用户在网页划选的句子文本..."           │  │ ← context-box (max-h 130px)
│ └────────────────────────────────────────┘  │
│                                              │
│                          ┌──────────────┐   │
│                          │ 用户提问原文  │   │ ← msg.user (蓝气泡, 右对齐)
│                          └──────────────┘   │
│ Absorb                                       │
│ 助手流式回答文本，无背景无边框，               │ ← msg.assistant (透明文本流)
│ 右贴边显示，闪烁光标▌                          │
│                                              │
│                  (滚动区，智能粘底)            │
├──────────────────────────────────────────────┤
│ ┌────────────────────────────────────────┐  │
│ │ 针对上面的句子提问...                    │  │ ← #questionInput
│ └────────────────────────────────────────┘  │
│ Enter 发送 · Shift+Enter 换行       [提问]   │ ← composer-actions
└──────────────────────────────────────────────┘
        ↑ 点「历史」时右侧滑入抽屉（z-index 10, width 80% max 320px）
        ↑ 点「⚙」时全屏遮罩 + 居中卡片（z-index 50）
```

---

## 附录：IDE 风格对话流设计依据

> 用户偏好（来自 user_profile）：
> - 「prefers IDE-like interface design for chat panels
>    (e.g., assistant responses as text flow without borders/background,
>     user messages with distinct bubbles, right-aligned alignment for both)」

| 维度 | 传统对话 UI | 本项目 IDE 风格 |
|---|---|---|
| 助手消息 | 灰底气泡 + 头像 | 透明文本流，无装饰 |
| 用户消息 | 灰底/主色气泡 + 头像 | 主色气泡，无头像 |
| 对齐 | 左右交错 | 整体右贴边（节省窄面板空间） |
| 角色标识 | 头像 + 名字 | 小字 role label（"我" / "Absorb"） |
| 视觉重心 | 双方对称 | 用户气泡强对比，助手弱对比（突出回答内容） |

**约束**：未来迭代禁止引入「助手气泡背景」「头像」「左右交错对齐」等设计，违反 §1 设计原则
