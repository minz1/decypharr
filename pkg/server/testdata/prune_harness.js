// Usage: node prune_harness.js <js dir> <config.html>
import assert from 'node:assert';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';

import {formatBytes, formatDuration} from '#ui/lib/common/format';
import {escapeHtml} from '#ui/lib/common/legacy-bridge';

const [jsDir, htmlPath] = process.argv.slice(2);
const pageIds = new Set([...fs.readFileSync(htmlPath, 'utf8').matchAll(/\sid="([^"$]+)"/g)].map((m) => m[1]));

const stub = new Proxy(function () {}, {
    get(_, key) {
        if (key === Symbol.toPrimitive) return () => '';
        if (key === 'then') return undefined;
        if (key === 'getElementById' || key === 'querySelector') return () => null;
        return stub;
    },
    apply: () => stub,
    construct: () => stub,
    set: () => true,
});
const ctx = vm.createContext({
    console, setTimeout, clearTimeout, URLSearchParams, FormData: class {},
    window: stub, document: stub, fetch: stub, navigator: stub, localStorage: stub, CustomEvent: stub,
});
vm.runInContext(fs.readFileSync(path.join(jsDir, 'config.js'), 'utf8'), ctx, {filename: 'config.js'});
const ConfigManager = vm.runInContext('ConfigManager', ctx);

class FakeElement {
    constructor() {
        this.value = '';
        this.checked = false;
        this.disabled = false;
        this.textContent = '';
        this.innerHTML = '';
        const classes = new Set();
        this.classList = {add: (c) => classes.add(c), remove: (c) => classes.delete(c), contains: (c) => classes.has(c)};
    }

    addEventListener() {}
}

function respond(status, body) {
    const text = typeof body === 'string' ? body : JSON.stringify(body);
    return {ok: status >= 200 && status < 300, status, text: async () => text, json: async () => JSON.parse(text)};
}

// routes: {'GET /api/config': response | Error | (() => Promise)}
function page(routes = {}, answers = []) {
    const elements = new Map();
    const requests = [];
    const toasts = [];
    const confirms = [];
    ctx.document = {
        getElementById(id) {
            if (!pageIds.has(id)) return null;
            if (!elements.has(id)) elements.set(id, new FakeElement());
            return elements.get(id);
        },
        querySelector: () => new FakeElement(),
    };
    const utils = {escapeHtml, formatBytes, formatDuration};
    utils.fetcher = async (url, options = {}) => {
        const key = `${options.method || 'GET'} ${url}`;
        requests.push({key, body: options.body});
        const route = routes[key];
        if (route instanceof Error) throw route;
        if (typeof route === 'function') return route();
        return route || respond(404, 'not found');
    };
    utils.createToast = (message, type) => toasts.push({message, type});
    ctx.window = {
        decypharrUtils: utils,
        location: {hash: '', reload() {}},
        confirm(message) {
            confirms.push(message);
            return answers.shift() ?? false;
        },
    };
    const m = Object.create(ConfigManager.prototype);
    m.refs = {loadingOverlay: new FakeElement()};
    return {m, el: (id) => ctx.document.getElementById(id), requests, toasts, confirms};
}

const same = (a, b) => assert.strictEqual(JSON.stringify(a), JSON.stringify(b));
const deletePrune = {enabled: true, mode: 'delete', schedule: '1h', grace: '72h', max_per_run: 5, roots: ['/data/a', '/data/b']};
const cases = {};

cases['populate then collect round-trips'] = () => {
    const {m} = page();
    m.populatePruneSettings(deletePrune);
    same(m.collectPruneConfig(), deletePrune);
    same(m.loadedPrune, deletePrune);
};

cases['defaults and unknown modes collect as report'] = () => {
    for (const prune of [undefined, {mode: 'report', max_per_run: 100}, {mode: 'nuke'}]) {
        const {m} = page();
        m.populatePruneSettings(prune);
        assert.strictEqual(m.collectPruneConfig().mode, 'report');
    }
};

cases['collectFormData sends prune only when changed'] = () => {
    const {m, el} = page();
    for (const key of Object.getOwnPropertyNames(ConfigManager.prototype)) {
        if (/^collect/.test(key) && !['collectFormData', 'collectPruneConfig'].includes(key)) m[key] = () => ({});
    }
    m.populatePruneSettings(deletePrune);
    assert.ok(!('prune' in JSON.parse(JSON.stringify(m.collectFormData()))), 'unchanged prune sent');
    el('prune.grace').value = '24h';
    same(m.collectFormData().prune, {...deletePrune, grace: '24h'});
    const fresh = page();
    for (const key of Object.keys(m)) if (typeof m[key] === 'function') fresh.m[key] = m[key];
    assert.ok(!('prune' in JSON.parse(JSON.stringify(fresh.m.collectFormData()))), 'prune sent without a baseline');
};

cases['confirmPruneDelete asks for every sent enabled delete prune'] = () => {
    const asks = (baseline, sent) => {
        const {m, confirms} = page();
        m.loadedPrune = baseline;
        m.confirmPruneDelete(sent);
        return confirms.length === 1;
    };
    const reportPrune = {...deletePrune, mode: 'report'};
    assert.ok(!asks(deletePrune, undefined), 'not sent');
    assert.ok(asks(reportPrune, deletePrune), 'switch to delete');
    assert.ok(asks({...deletePrune, enabled: false}, deletePrune), 'enable with delete stored');
    assert.ok(asks(deletePrune, {...deletePrune, grace: '24h'}), 'shorter grace');
    assert.ok(asks(deletePrune, {...deletePrune, max_per_run: 50}), 'higher max_per_run');
    assert.ok(asks(deletePrune, {...deletePrune, roots: ['/data/a']}), 'root removed');
    assert.ok(!asks(deletePrune, {...deletePrune, enabled: false}), 'disabled delete');
    assert.ok(!asks(deletePrune, reportPrune), 'enabled report');
};

cases['the delete confirm names the mode, max_per_run and grace'] = () => {
    const {m, confirms} = page();
    m.confirmPruneDelete(deletePrune);
    assert.match(confirms[0], /delete/);
    assert.match(confirms[0], /\b5\b/);
    assert.match(confirms[0], /72h/);
};

async function save(answer) {
    const p = page({'POST /api/config': respond(200, {restarted: false})}, [answer]);
    p.m.loaded = true;
    p.m.collectFormData = () => ({prune: deletePrune});
    p.m.validateConfiguration = () => ({valid: true, errors: []});
    await p.m.saveConfiguration({preventDefault() {}});
    return p;
}

async function saveAfterLoad(getConfig) {
    const p = page({'GET /api/config': getConfig, 'POST /api/config': respond(200, {restarted: false})});
    p.m.populateForm = () => {};
    await p.m.loadConfiguration();
    p.m.collectFormData = () => ({});
    p.m.validateConfiguration = () => ({valid: true, errors: []});
    await p.m.saveConfiguration({preventDefault() {}});
    return p;
}

cases['a save after a failed config load posts nothing'] = async () => {
    for (const getConfig of [respond(500, 'down'), new Error('offline')]) {
        const {requests, toasts} = await saveAfterLoad(getConfig);
        same(requests.map((r) => r.key), ['GET /api/config']);
        assert.ok(toasts.some((t) => t.type === 'error' && /reload/i.test(t.message)), 'no reload hint');
    }
};

cases['a save after a successful config load posts'] = async () => {
    const {requests} = await saveAfterLoad(respond(200, {}));
    same(requests.map((r) => r.key), ['GET /api/config', 'POST /api/config']);
};

cases['a cancelled delete confirm posts nothing'] = async () => {
    const {m, requests, confirms} = await save(false);
    assert.strictEqual(confirms.length, 1);
    same(requests, []);
    assert.ok(m.refs.loadingOverlay.classList.contains('hidden'));
};

cases['an accepted delete confirm posts once and moves the baseline'] = async () => {
    const {m, requests} = await save(true);
    same(requests.map((r) => r.key), ['POST /api/config']);
    same(JSON.parse(requests[0].body).prune, deletePrune);
    same(m.loadedPrune, deletePrune);
};

async function run(routes, answers = []) {
    const p = page(routes, answers);
    p.m.loadedPrune = {...deletePrune, mode: 'report'};
    await p.m.runPruneNow();
    return p;
}

const report = {started_at: '2026-10-24T12:00:00Z', mode: 'report', aborted: '', candidates: []};

cases['run now confirms when the saved config is delete'] = async () => {
    const {requests, confirms} = await run({'GET /api/config': respond(200, {prune: deletePrune})}, [false]);
    assert.strictEqual(confirms.length, 1);
    same(requests.map((r) => r.key), ['GET /api/config']);
};

cases['run now posts after an accepted delete confirm'] = async () => {
    const {requests} = await run({
        'GET /api/config': respond(200, {prune: deletePrune}),
        'POST /api/prune/run': respond(200, report),
    }, [true]);
    same(requests.map((r) => r.key), ['GET /api/config', 'POST /api/prune/run']);
};

cases['run now aborts when the config read fails'] = async () => {
    for (const failure of [new Error('offline'), respond(401, {error: 'unauthorized'})]) {
        const {requests, toasts} = await run({'GET /api/config': failure});
        same(requests.map((r) => r.key), ['GET /api/config']);
        assert.strictEqual(toasts[0].type, 'error');
    }
};

cases['run now in report mode posts without a confirm'] = async () => {
    const p = page({
        'GET /api/config': respond(200, {prune: {...deletePrune, mode: 'report'}}),
        'POST /api/prune/run': () => {
            assert.ok(p.el('pruneRunBtn').disabled, 'button enabled during the run');
            return respond(200, report);
        },
    });
    p.m.loadedPrune = deletePrune;
    await p.m.runPruneNow();
    same(p.confirms, []);
    assert.ok(!p.el('pruneRunBtn').disabled, 'button left disabled');
    assert.match(p.el('pruneStatus').innerHTML, /report/);
};

cases['run now reports the server errors'] = async () => {
    const reportConfig = {'GET /api/config': respond(200, {prune: {enabled: true, mode: 'report'}})};
    let p = await run({...reportConfig, 'POST /api/prune/run': respond(409, {error: 'prune is disabled'})});
    assert.match(p.toasts[0].message, /Prune is disabled: enable it and save first/);
    p = await run({...reportConfig, 'POST /api/prune/run': respond(500, {error: 'scan failed: boom'})});
    assert.match(p.toasts[0].message, /scan failed: boom/);
    p = await run({...reportConfig, 'POST /api/prune/run': respond(200, {...report, aborted: 'baseline dropped'})});
    assert.strictEqual(p.toasts[0].type, 'warning');
    assert.match(p.toasts[0].message, /aborted: baseline dropped/);
    p = await run({...reportConfig, 'POST /api/prune/run': new Error('network')});
    assert.match(p.toasts[0].message, /may still be in progress/);
    assert.ok(!p.el('pruneRunBtn').disabled);
};

const xss = '<img src=x onerror=1>';

cases['status renders server strings as text'] = () => {
    const {m, el} = page();
    m.renderPruneStatus({
        ...report, aborted: xss, mode: xss,
        candidates: [
            {name: xss, provider: xss, excluded: '', size: 10, eligible_at: '2026-10-20T00:00:00Z'},
            {name: 'b', provider: 'rd', excluded: xss, size: 0},
        ],
    });
    assert.ok(!el('pruneStatus').innerHTML.includes('<img'), el('pruneStatus').innerHTML);
    assert.match(el('pruneStatus').innerHTML, /&lt;img/);
};

cases['an aborted run with null candidates shows the reason'] = () => {
    const {m, el} = page();
    m.renderPruneStatus({...report, aborted: 'referenced fell below 90%', candidates: null});
    assert.match(el('pruneStatus').innerHTML, /referenced fell below 90%/);
};

cases['no run yet'] = () => {
    const {m, el} = page();
    m.renderPruneStatus({mode: 'report', candidates: null});
    assert.match(el('pruneStatus').innerHTML, /No run since decypharr started/);
};

cases['eligibility compares instants, not strings'] = () => {
    const {m, el} = page();
    m.renderPruneStatus({
        ...report, started_at: '2026-10-24T12:00:00+02:00',
        candidates: [{name: 'late', provider: 'rd', excluded: '', size: 1, eligible_at: '2026-10-24T11:00:00Z'}],
    });
    assert.match(el('pruneStatus').innerHTML, /Eligible<\/dt>\s*<dd[^>]*>0</);
};

cases['candidates sort eligible, waiting, excluded and cap at 100'] = () => {
    const {m, el} = page();
    const waiting = (i) => ({name: `waiting-${i}`, provider: 'rd', excluded: '', size: 1, eligible_at: `2026-10-25T00:00:${String(i).padStart(2, '0')}Z`});
    m.renderPruneStatus({
        ...report,
        candidates: [
            {name: 'excluded-1', provider: 'rd', excluded: 'manual', size: 1},
            waiting(9), waiting(1),
            {name: 'eligible-1', provider: 'rd', excluded: '', size: 1, eligible_at: '2026-10-20T00:00:00Z'},
            ...Array.from({length: 100}, (_, i) => ({name: `excluded-x${i}`, provider: 'rd', excluded: 'queued', size: 1})),
        ],
    });
    const html = el('pruneStatus').innerHTML;
    const order = ['eligible-1', 'waiting-1', 'waiting-9', 'excluded-1'].map((n) => html.indexOf(`>${n}<`));
    assert.ok(order.every((at, i) => at > 0 && (i === 0 || at > order[i - 1])), JSON.stringify(order));
    assert.match(html, /and 4 more/);
    assert.ok(!html.includes('>excluded-x96<') && html.includes('>excluded-x95<'), 'not capped at 100 rows');
};

(async () => {
    for (const [name, fn] of Object.entries(cases)) {
        try {
            await fn();
        } catch (error) {
            console.error(`FAIL ${name}: ${error.stack}`);
            process.exitCode = 1;
        }
    }
})();
