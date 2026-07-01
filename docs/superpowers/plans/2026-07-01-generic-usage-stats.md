# Generic Usage Stats Modal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the existing `#quotaChartModal` reachable from every provider (not only MiniMax quota-enabled ones) via a 📊 button in `provider-item-stats`; rename its semantics from "限额趋势" to "用量统计"; preserve MiniMax-only behavior (extra 使用率 series + 限额 footer + 限额拦截 toggle) via runtime `hasQuota` detection; keep both WSS-driven quota (10s) and per-event stats (per request) live-refreshing the chart while the modal is open.

**Architecture:**
- **Backend: zero changes.** Reuses existing `GET /api/providers/:id/token-history` (all providers) and `GET /api/providers/:id/quota-history` (returns empty for non-MiniMax, valid behavior).
- **Frontend only:** `web/static/index.html` — DOM button + state rename + WSS hook for per-event stats push + chart option built around a `hasQuota` flag.
- `hasQuota = p.quota_enabled && p.quota_interval && p.quota_interval.enabled` — runtime signal, no provider-type hardcoding.
- Old DOM ids (`quotaChartModal`, `quotaChart`, `quotaRangeToggle`, `quotaChartTitle`, `quotaChartCurrent`, `quotaChartReset`, `quotaChartPerPercent`) are preserved to keep diff small; JS state object renamed `quotaChartState` → `usageChartState`.

**Tech Stack:** Vanilla JS + ECharts (already vendored at `web/static/vendor/echarts.min.js`).

**Spec:** `docs/superpowers/specs/2026-07-01-generic-usage-stats-design.md`

---

## File Structure

| File | Responsibility |
|------|----------------|
| `web/static/index.html` *(modify)* | Provider-card 📊 button · `showUsageStatsModal` / `loadUsageChart` / `buildUsageChartOption` · `usageChartState` rename · WSS `applyStatsUpdate` wrapper · `.usage-stats-btn` CSS · title text `用量统计 - {name}` |

Only one file changes. The change is purely client-side: server endpoints, DB schema, and other packages are untouched.

---

## Task 1: Add 📊 button to provider-item-stats

**Files:**
- Modify: `web/static/index.html:1666-1708` (the `renderProviders` template literal that renders each provider card)

- [ ] **Step 1: Locate the end of `provider-item-stats`**

Find the `renderProviders` function and the line `</div>` that closes `provider-item-stats`. The structure is:

```js
list.innerHTML = providers.map(p => `
    <div class="provider-item ${p.is_active ? 'active' : ''}">
        <div class="provider-item-header">
            ...
        </div>
        <div class="provider-item-stats">
            <div class="provider-stat provider-stat-models">
                ${renderModelFormatStat(p)}
            </div>
            <div class="provider-stat quota-stat" data-pid="${p.id}" ...>
                ${renderQuotaBars(p)}
                <label class="quota-block-toggle" ...>...</label>
            </div>
            <!-- ↑↑↑ 这里就是 provider-item-stats 闭合 ↑↑↑ -->
        </div>
        <div class="provider-item-actions">
            ...
        </div>
    </div>
`).join('');
```

Confirm exact location by reading the surrounding lines (line numbers may shift between reads).

- [ ] **Step 2: Insert the 📊 button**

Inside the template, **after the closing `</div>` of `provider-stat quota-stat`** and **before the closing `</div>` of `provider-item-stats`**, append:

```html
            <button type="button" class="usage-stats-btn"
                    data-pid="${p.id}" title="用量统计"
                    onclick="showUsageStatsModal('${p.id}')">
                📊 用量统计
            </button>
```

Expected final shape:

```html
        <div class="provider-item-stats">
            <div class="provider-stat provider-stat-models">
                ${renderModelFormatStat(p)}
            </div>
            <div class="provider-stat quota-stat" data-pid="${p.id}" ...>
                ...
            </div>
            <button type="button" class="usage-stats-btn"
                    data-pid="${p.id}" title="用量统计"
                    onclick="showUsageStatsModal('${p.id}')">
                📊 用量统计
            </button>
        </div>
```

- [ ] **Step 3: Add CSS for `.usage-stats-btn`**

In the `<style>` block (after the existing `.quota-bar-right .pct.quota-red` rule around line 247), append:

```css
.usage-stats-btn {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 6px 12px;
    margin-top: 6px;
    font-size: 12px;
    color: #667eea;
    background: #f7fafc;
    border: 1px solid #cbd5e0;
    border-radius: 4px;
    cursor: pointer;
    transition: all 150ms;
    align-self: flex-start;
}
.usage-stats-btn:hover {
    background: #667eea;
    color: white;
    border-color: #667eea;
}
```

- [ ] **Step 4: Verify file parses**

Run:
```bash
node -e "const fs=require('fs'); const html=fs.readFileSync('web/static/index.html','utf8'); const scripts=[...html.matchAll(/<script(?![^>]*src=)[^>]*>([\s\S]*?)<\/script>/g)].map(m=>m[1]); console.log('found '+scripts.length+' inline scripts'); scripts.forEach((s,i)=>{try{new Function(s);console.log('script '+i+' OK')}catch(e){console.log('script '+i+' SYNTAX ERROR: '+e.message)}})"
```

Expected: `found N inline scripts` and every line ends with `OK`.

- [ ] **Step 5: Commit**

```bash
git add web/static/index.html
git commit -m "feat(web): add 📊 button on provider cards for usage stats modal"
```

---

## Task 2: Rename modal title + state object to `usageChartState`

**Files:**
- Modify: `web/static/index.html` (around line 593 — modal title — and line 2289 — JS state declaration)

- [ ] **Step 1: Update modal default title text**

Find `<h3 id="quotaChartTitle">限额趋势</h3>` (around line 593). Change it to:

```html
                <h3 id="quotaChartTitle">用量统计</h3>
```

(The id stays `quotaChartTitle` per spec §6 to minimize diff — we'll set its `textContent` dynamically anyway.)

- [ ] **Step 2: Rename the JS state object**

Find `let quotaChartState = null; // {chart, pid, window, range, reqSeq}` (around line 2289). Replace with:

```js
        // --- Usage Stats Modal ---
        // usageChartState = {chart, pid, window, range, reqSeq, hasQuota, resizeObserver}
        // - hasQuota: when true, the modal additionally shows the 使用率 series,
        //   the left % Y-axis, the current-percent markLine, and the 限额 footer.
        //   Computed at open time from p.quota_enabled && p.quota_interval.enabled.
        let usageChartState = null;
```

- [ ] **Step 3: Replace all in-file references to `quotaChartState`**

Use grep to enumerate every reference first:

```bash
grep -n "quotaChartState" web/static/index.html
```

For every occurrence outside the spec's "preserved id" list (the modal DOM ids are `quotaChartModal`, `quotaChart`, `quotaRangeToggle`, `quotaChartTitle`, `quotaChartCurrent`, `quotaChartReset`, `quotaChartPerPercent` — those IDs are intentional and stay), replace `quotaChartState` with `usageChartState`.

Expected references (search and confirm each):
- the `let` declaration (already done in step 2)
- inside `showQuotaChartModal` / `loadQuotaChart` body (multiple reads/writes)
- inside the `applyQuotaUpdate` wrapper at the bottom of the file

Use `Edit` tool with `replace_all: true` on the JS identifier (the regex `quotaChartState` is unique to this state object — no false-positive risk because no DOM id matches).

- [ ] **Step 4: Verify file parses**

Re-run the same `node -e` script from Task 1 Step 4. Expected: all scripts OK.

- [ ] **Step 5: Commit**

```bash
git add web/static/index.html
git commit -m "refactor(web): rename quotaChartState → usageChartState (modal is now generic)"
```

---

## Task 3: Convert `showQuotaChartModal` → `showUsageStatsModal`

**Files:**
- Modify: `web/static/index.html:2291-2334` (the existing `showQuotaChartModal` function)

- [ ] **Step 1: Replace the function body**

Find the `function showQuotaChartModal(pid, window) { ... }` block. Replace the **entire function** with:

```js
        function showUsageStatsModal(pid, opts = {}) {
            const p = providers.find(x => x.id === pid);
            if (!p) return;

            // 标题固定为「用量统计 - {name}」,通用语义,不再追加 5h/周限额字样。
            document.getElementById('quotaChartTitle').textContent = `用量统计 - ${p.name}`;

            const modal = document.getElementById('quotaChartModal');
            modal.classList.add('show');

            // 计算 hasQuota:有 quota_enabled 且 interval 窗口 enabled 才算"有使用率数据可画"。
            // 这是运行时信号,不在前端硬编码"该 provider 是否 MiniMax"。
            const hasQuota = !!(p.quota_enabled
                            && p.quota_interval
                            && p.quota_interval.enabled);

            // 如果已经是同一个 provider 的同一 window,只刷新当前 range(避免重建 echarts)。
            if (usageChartState
                && usageChartState.pid === pid
                && usageChartState.window === (opts.window || 'interval')) {
                loadUsageChart(usageChartState.range);
                return;
            }

            // 切换 provider / window:dispose 旧 chart 并解绑旧 observer。
            if (usageChartState) {
                if (usageChartState.resizeObserver) usageChartState.resizeObserver.disconnect();
                usageChartState.chart.dispose();
            }

            const chartEl = document.getElementById('quotaChart');
            chartEl.innerHTML = '';
            const chart = echarts.init(chartEl, null, { renderer: 'canvas' });
            const resizeObserver = new ResizeObserver(() => {
                if (usageChartState && usageChartState.chart === chart && !chart.isDisposed()) {
                    chart.resize();
                }
            });
            resizeObserver.observe(chartEl);
            usageChartState = {
                chart,
                pid,
                window: opts.window || 'interval',
                range: '5h',
                reqSeq: 0,
                hasQuota,
                resizeObserver,
            };

            // range toggle
            const toggle = document.getElementById('quotaRangeToggle');
            toggle.querySelectorAll('button').forEach(b => {
                b.onclick = () => {
                    toggle.querySelectorAll('button').forEach(x => x.classList.remove('active'));
                    b.classList.add('active');
                    usageChartState.range = b.dataset.range;
                    loadUsageChart(b.dataset.range);
                };
            });

            loadUsageChart('5h');
        }

        // 保留旧名作为薄包装,让 quota-bar-row 的 click 委托继续可用。
        function showQuotaChartModal(pid, window) {
            showUsageStatsModal(pid, { window });
        }
```

- [ ] **Step 2: Verify file parses**

Re-run the `node -e` parse check. Expected: all scripts OK.

- [ ] **Step 3: Commit**

```bash
git add web/static/index.html
git commit -m "feat(web): introduce showUsageStatsModal, keep showQuotaChartModal as wrapper"
```

---

## Task 4: Convert `loadQuotaChart` → `loadUsageChart` with `hasQuota` branching

**Files:**
- Modify: `web/static/index.html:2345-2379` (existing `loadQuotaChart` function)
- Modify: `web/static/index.html:2381-2429` (existing `buildQuotaChartOption` function)

- [ ] **Step 1: Replace `loadQuotaChart`**

Find `async function loadQuotaChart(range) { ... }`. Replace the entire function with:

```js
        async function loadUsageChart(range) {
            if (!usageChartState) return;
            const { chart, pid, window, hasQuota, reqSeq } = usageChartState;
            const mySeq = usageChartState.reqSeq = reqSeq + 1;

            // 始终拉 token-history;仅当 hasQuota 才拉 quota-history。
            // 两条请求互不依赖,并行。
            const tokenP = fetch(`/api/providers/${pid}/token-history?range=${range}`).then(r => {
                if (!r.ok) throw new Error(`token-history HTTP ${r.status}`);
                return r.json();
            });
            const quotaP = hasQuota
                ? fetch(`/api/providers/${pid}/quota-history?window=${window}&range=${range}`)
                    .then(r => {
                        if (!r.ok) throw new Error(`quota-history HTTP ${r.status}`);
                        return r.json();
                    })
                : Promise.resolve({ points: [], current: {} });

            try {
                const [tokenData, quotaData] = await Promise.all([tokenP, quotaP]);
                if (mySeq !== usageChartState.reqSeq) return; // 被新请求取代,丢弃

                // 空数据:画布占位 + footer 提示
                const tokenPts = tokenData.points || [];
                const quotaPts = hasQuota ? (quotaData.points || []) : [];
                if (!tokenPts.length && !quotaPts.length) {
                    chart.setOption({
                        title: { text: '暂无用量记录,请先使用一次', left: 'center', top: 'center',
                                 textStyle: { color: '#999', fontSize: 14 } }
                    }, true);
                    document.getElementById('quotaChartCurrent').textContent = '暂无数据';
                    document.getElementById('quotaChartReset').textContent = '';
                    document.getElementById('quotaChartPerPercent').textContent = '';
                    return;
                }

                const current = quotaData.current || {};
                const currentPercent = hasQuota ? current.used_percent : null;
                const perPercent = hasQuota
                    ? computePerPercentTokens(quotaPts, tokenPts, currentPercent)
                    : '';

                chart.setOption(buildUsageChartOption(quotaPts, tokenPts, range, currentPercent, hasQuota), true);

                // footer
                document.getElementById('quotaChartCurrent').textContent =
                    hasQuota && currentPercent != null ? `当前: ${currentPercent.toFixed(1)}%` : '';
                document.getElementById('quotaChartReset').textContent =
                    hasQuota && current.reset_in_human ? `下次重置: ${current.reset_in_human}` : '';
                document.getElementById('quotaChartPerPercent').textContent = perPercent;
            } catch (err) {
                if (mySeq !== usageChartState.reqSeq) return;
                chart.setOption({
                    title: { text: `加载失败: ${err.message}`, left: 'center', top: 'center',
                             textStyle: { color: '#e53e3e', fontSize: 14 } }
                }, true);
            }
        }
```

- [ ] **Step 2: Replace `buildQuotaChartOption`**

Find `function buildQuotaChartOption(quotaData, tokenData, range, currentPercent) { ... }`. Replace with:

```js
        function buildUsageChartOption(quotaPoints, tokenPoints, range, currentPercent, hasQuota) {
            const xAxisRange = { '5h': 5*3600*1000, '1h': 3600*1000, '7d': 7*24*3600*1000 };
            const nowMs = Date.now();
            const startMs = nowMs - xAxisRange[range];

            // 使用率系列的颜色来自最后一个 quota 点,与现状一致;无 quota 时用中性灰色。
            let quotaColor = '#22d3ee'; // 默认 cyan
            let markLines = [];
            let leftAxis = null;
            if (hasQuota) {
                const lastPct = quotaPoints.length
                    ? quotaPoints[quotaPoints.length - 1].used_percent
                    : 0;
                const qClass = quotaBarClass(lastPct);
                const colorMap = { cyan: '#22d3ee', orange: '#e67e22', redOrange: '#e67e22', red: '#c0392b' };
                quotaColor = colorMap[qClass] || '#22d3ee';
                leftAxis = {
                    type: 'value', name: '使用率 %', min: 0, max: 100, position: 'left',
                    axisLabel: { formatter: '{value}%' }
                };
                if (currentPercent != null && currentPercent >= 0 && currentPercent <= 100) {
                    markLines = [{
                        yAxis: currentPercent,
                        lineStyle: { color: quotaColor, type: 'dashed', width: 1.5 },
                        label: { formatter: `当前 ${currentPercent.toFixed(1)}%`, position: 'insideEndTop', color: quotaColor, fontSize: 11 },
                    }];
                }
            }

            const yAxes = [
                leftAxis,
                { type: 'value', name: 'Token', position: 'right',
                  axisLabel: { formatter: v => formatTokenCount(v) } },
            ].filter(Boolean); // 没有 hasQuota 时不画左轴

            // legend 只显示实际画的系列
            const legendData = hasQuota
                ? ['使用率', '输入', '输出', '总Token']
                : ['输入', '输出', '总Token'];

            const series = [
                { name: '输入', type: 'line', yAxisIndex: hasQuota ? 1 : 0, smooth: true, connectNulls: true, showSymbol: false,
                  itemStyle: { color: '#3b82f6' },
                  data: tokenPoints.map(p => [p.t * 1000, p.input_tokens]) },
                { name: '输出', type: 'line', yAxisIndex: hasQuota ? 1 : 0, smooth: true, connectNulls: true, showSymbol: false,
                  itemStyle: { color: '#a855f7' },
                  data: tokenPoints.map(p => [p.t * 1000, p.output_tokens]) },
                { name: '总Token', type: 'line', yAxisIndex: hasQuota ? 1 : 0, smooth: true, connectNulls: true, showSymbol: false,
                  lineStyle: { width: 3 },
                  itemStyle: { color: '#111827' },
                  data: tokenPoints.map(p => [p.t * 1000, p.total_tokens]) },
            ];

            if (hasQuota) {
                series.unshift({
                    name: '使用率', type: 'line', yAxisIndex: 0, smooth: true,
                    connectNulls: true, showSymbol: false,
                    itemStyle: { color: quotaColor },
                    areaStyle: { color: quotaColor, opacity: 0.15 },
                    markLine: { silent: true, symbol: 'none', data: markLines },
                    data: quotaPoints.map(p => [p.t * 1000, p.used_percent]),
                });
            }

            return {
                grid: { left: 60, right: 70, top: 40, bottom: 50 },
                tooltip: { trigger: 'axis', axisPointer: { type: 'cross' } },
                legend: { top: 4, data: legendData },
                xAxis: { type: 'time', min: startMs, max: nowMs },
                yAxis: yAxes,
                series,
            };
        }
```

- [ ] **Step 3: Replace remaining references to `loadQuotaChart`**

Use grep:
```bash
grep -n "loadQuotaChart" web/static/index.html
```

All references must be replaced with `loadUsageChart`:
- inside `showUsageStatsModal` (3 references)
- inside `showQuotaChartModal` wrapper if any
- inside the `applyQuotaUpdate` override at the bottom

Use `Edit` with `replace_all: true` on the JS identifier.

- [ ] **Step 4: Verify file parses**

Re-run the `node -e` parse check. Expected: all scripts OK.

- [ ] **Step 5: Commit**

```bash
git add web/static/index.html
git commit -m "feat(web): split chart option by hasQuota — non-quota providers get 3-series, quota providers keep 4-series"
```

---

## Task 5: Wire stats push to refresh the modal

**Files:**
- Modify: `web/static/index.html:1448-1452` (inside `ws.onmessage`, the per-event branch `data.input_tokens !== undefined`)

- [ ] **Step 1: Read the current per-event branch**

Currently:

```js
                } else if (data.input_tokens !== undefined) {
                    console.log('New token usage received, refreshing stats...');
                    loadStats();
                    loadDailyStats();
                }
```

This branch fires on every per-event stats push from `stats.RecordUsage`. The payload includes `provider_id` (verified — `UsageRecord.ProviderID` JSON tag = `provider_id`).

- [ ] **Step 2: Add the modal-refresh trigger**

Replace the entire branch with:

```js
                } else if (data.input_tokens !== undefined) {
                    console.log('New token usage received, refreshing stats...');
                    loadStats();
                    loadDailyStats();
                    // 打开中的用量统计 modal:若本次 push 来自当前 modal 的 provider,自动 reload,
                    // 让 token 曲线随每次请求实时刷新。
                    if (usageChartState && data.provider_id === usageChartState.pid) {
                        loadUsageChart(usageChartState.range);
                    }
                }
```

- [ ] **Step 3: Update the quota WSS wrapper**

Find (around line 2477–2483):

```js
        const _origApplyQuotaUpdate = applyQuotaUpdate;
        applyQuotaUpdate = function(quotas) {
            _origApplyQuotaUpdate(quotas);
            if (quotaChartState && quotas[quotaChartState.pid]) {
                loadQuotaChart(quotaChartState.range);
            }
        };
```

Replace with:

```js
        const _origApplyQuotaUpdate = applyQuotaUpdate;
        applyQuotaUpdate = function(quotas) {
            _origApplyQuotaUpdate(quotas);
            if (usageChartState && quotas[usageChartState.pid]) {
                loadUsageChart(usageChartState.range);
            }
        };
```

(Note: `quotaChartState` → `usageChartState`, `loadQuotaChart` → `loadUsageChart`.)

- [ ] **Step 4: Verify file parses**

Re-run the `node -e` parse check. Expected: all scripts OK.

- [ ] **Step 5: Commit**

```bash
git add web/static/index.html
git commit -m "feat(web): refresh usage stats modal on per-event stats push + quota push"
```

---

## Task 6: Hide modal + dispose on close (verify lifecycle)

**Files:**
- Modify: `web/static/index.html:2336-2343` (existing `hideQuotaChartModal` function)

- [ ] **Step 1: Read current hide function**

```js
function hideQuotaChartModal() {
    document.getElementById('quotaChartModal').classList.remove('show');
    if (quotaChartState) {
        if (quotaChartState.resizeObserver) quotaChartState.resizeObserver.disconnect();
        quotaChartState.chart.dispose();
        quotaChartState = null;
    }
}
```

(Reference: existing code already at line ~2336.)

- [ ] **Step 2: Rename state reference**

Replace `quotaChartState` with `usageChartState` (3 occurrences) using `Edit` with `replace_all: true` if no other variable named `quotaChartState` exists in this function (should be true after Task 2 already renamed the declaration).

After replacement, the function should read:

```js
function hideQuotaChartModal() {
    document.getElementById('quotaChartModal').classList.remove('show');
    if (usageChartState) {
        if (usageChartState.resizeObserver) usageChartState.resizeObserver.disconnect();
        usageChartState.chart.dispose();
        usageChartState = null;
    }
}
```

- [ ] **Step 3: Verify file parses**

Re-run the `node -e` parse check. Expected: all scripts OK.

- [ ] **Step 4: Confirm `usageChartState` is fully consistent**

Run:
```bash
grep -n "quotaChartState" web/static/index.html
```

Expected: **no matches** (all renamed in earlier tasks).

```bash
grep -n "loadQuotaChart" web/static/index.html
```

Expected: **no matches** (all renamed in Task 4).

```bash
grep -n "showQuotaChartModal" web/static/index.html
```

Expected: **1 match** in the wrapper at the bottom of the modal block (kept on purpose so the existing click delegation for `.quota-bar-row[data-pid]` continues to work).

- [ ] **Step 5: Commit**

```bash
git add web/static/index.html
git commit -m "chore(web): align hideQuotaChartModal with renamed usageChartState"
```

---

## Task 7: Manual verification per spec §9.2

**Files:** none (browser-only check)

- [ ] **Step 1: Start the server**

Run: `go run .`
Expected: server starts on `:8080`, no compile errors.

- [ ] **Step 2: Open browser to `http://localhost:8080/`**

Log in with the configured TOTP code.

- [ ] **Step 3: Verify 📊 button appears on every provider card**

Check: every provider card (active and inactive) shows the 📊 用量统计 button regardless of whether it has quota enabled.

- [ ] **Step 4: Click 📊 on a non-MiniMax provider**

Expected:
- Modal title: `用量统计 - {name}`
- 3 series in legend: `输入`, `输出`, `总Token`
- No left Y-axis labeled `使用率 %`
- No current% markLine
- Footer `quotaChartCurrent` / `quotaChartReset` / `quotaChartPerPercent` are empty

- [ ] **Step 5: Click 📊 on a MiniMax provider with quota_enabled=true**

Expected:
- 4 series in legend: `使用率`, `输入`, `输出`, `总Token`
- Left Y-axis labeled `使用率 %` 0–100
- A horizontal markLine at the current used_percent
- Footer shows `当前: X.X%` and `每 1% ≈ X token` (after some token usage has accumulated)

- [ ] **Step 6: Click 📊 on a MiniMax provider with quota_enabled=false (e.g., quota fetch failing)**

Expected: same as Step 4 (3-series, no quota info). The `hasQuota` runtime signal correctly downgrades the view.

- [ ] **Step 7: Click an existing `.quota-bar-row[data-window="interval"]`**

Expected: same modal opens with title `用量统计 - {name}` (not `5h 限额趋势 - {name}` — title is unified). The 使用率 markLine points to the interval window's current value.

- [ ] **Step 8: New provider cold-start**

Add a new provider, do NOT trigger any request, then click 📊 on it.

Expected: echarts displays the centered text `暂无用量记录,请先使用一次`. Footer `quotaChartCurrent` reads `暂无数据`.

- [ ] **Step 9: Live refresh — token curve**

While the modal is open on any provider, send a real request through the proxy (use the existing `/log.html` page or any test client) that uses the same provider. Watch the Network tab.

Expected: a new `GET /api/providers/{pid}/token-history?range=...` request fires immediately after the stats push arrives. The chart redraws with the new point.

- [ ] **Step 10: Live refresh — quota curve**

While the modal is open on a MiniMax provider, wait ~10 seconds.

Expected: every ~10s, the chart refetches. The 使用率 markLine position moves to the latest value reported by the upstream quota poll.

- [ ] **Step 11: Close modal**

Click `×` or the backdrop.

Expected: modal hides. Verify via DevTools Memory profile or just by re-opening: the previous chart instance is gone (no leak).

- [ ] **Step 12: No regression — overall stats page still works**

Visit `/log.html`, navigate the table, switch keys, etc. Confirm no console errors and existing charts still render.

---

## Task 8: Final test sweep + summary

**Files:** none (verification only)

- [ ] **Step 1: Run `go test ./...`**

```bash
go test ./...
```

Expected: all PASS (no backend changes, but confirm nothing broke).

- [ ] **Step 2: Verify no stray console errors**

Open DevTools console, reload `http://localhost:8080/`, exercise both quota and non-quota providers. Expected: no JavaScript errors, only the existing `console.log` lines for WebSocket and stats events.

- [ ] **Step 3: Update memory if non-obvious facts emerged**

If during implementation you discovered something surprising (e.g., a WSS payload field that's not what the spec assumed), save it to memory via the `Write` tool at `C:\Users\Admin\.claude\projects\c--Users-Admin-src-switchai\memory\<slug>.md` and add a one-line pointer to `MEMORY.md`. Otherwise skip.

- [ ] **Step 4: Final commit (cleanup, if any)**

```bash
git status
```

If clean → nothing to commit. If any leftover changes (e.g., trailing whitespace fixes during the manual test pass):

```bash
git add -A && git commit -m "chore: post-implementation cleanup"
```

- [ ] **Step 5: Print summary to user**

Tell the user:
- Which commits were made
- That the manual verification checklist passed
- That no backend / DB / API changes occurred
- That they can `git log --oneline` to review

---

## Self-Review Notes

- Spec §1 目标 (generic modal, all providers) → Task 1 (📊 button) + Task 3 (`showUsageStatsModal`)
- Spec §3 用户视角流程 → Tasks 1, 3, 4, 5 (entry, modal flow, empty-data, WSS hooks)
- Spec §4 实时刷新 → Task 5 (stats push + quota push hooks) + Task 3 (`usageChartState` lifecycle)
- Spec §5 改动范围 → Tasks 1–6 (single-file, all 10 changes enumerated)
- Spec §6 ID 重命名策略 → Task 2 (DOM ids preserved, JS state renamed)
- Spec §7 数据流 → Tasks 3, 4 (state init, fetch branching)
- Spec §8 hasQuota 判定 → Task 3 step 1 (runtime signal, no provider-type hardcoding)
- Spec §9 测试 → Tasks 7 (manual browser checklist) + Task 8 (go test ./...)
- Spec §11 不做什么 → respected throughout: no cost chart, no backend changes, no range change, no multi-provider overlay

Identified single risk: if a future stats payload field renames `provider_id`, the WSS hook in Task 5 silently stops refreshing. Mitigation: the modal still works on range switches; only live per-event refresh degrades. Documented here so future maintainers know to check the JSON tag if they tweak `UsageRecord`.
