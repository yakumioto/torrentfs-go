// ==UserScript==
// @name         TorrentFS M-Team bridge
// @namespace    https://github.com/yakumioto/torrentfs-go
// @version      0.2.0
// @description  Send a torrent from an M-Team detail page to a configured TorrentFS instance.
// @match        https://m-team.cc/*
// @match        https://*.m-team.cc/*
// @match        https://m-team.io/*
// @match        https://*.m-team.io/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_deleteValue
// @grant        GM_registerMenuCommand
// @grant        GM_addStyle
// @grant        GM_addElement
// @connect      *
// @sandbox      DOM
// @run-at       document-start
// @license      MPL-2.0
// ==/UserScript==

(function () {
    'use strict';

    const STORAGE_KEY = 'torrentfs.connection';
    const MAX_TORRENT_BYTES = 10 * 1024 * 1024;
    const REQUEST_TIMEOUT = 30 * 1000;
    const BRIDGE_SOURCE = 'torrentfs-mteam-bridge';
    const pageWindow = window;
    // Capture sandbox dialog functions before any page event handler runs; they return browser UI values, not page DOM data.
    const sandboxPrompt = typeof globalThis.prompt === 'function' ? globalThis.prompt.bind(globalThis) : null;
    const sandboxConfirm = typeof globalThis.confirm === 'function' ? globalThis.confirm.bind(globalThis) : null;

    const MTEAM = {
        detailPath: '/api/torrent/detail',
        downloadTokenPath: '/torrent/genDlToken',
        hosts: ['m-team.cc', 'm-team.io'],
        selectors: {
            floatMount: '#float-btns',
            nativeDownload: 'button.ant-btn-primary'
        },
        matchesPage() {
            return Boolean(parseDetailRoute() || isDetailRoutePath());
        },
        isHost(host) {
            return this.hosts.some((domain) => host === domain || host.endsWith(`.${domain}`));
        }
    };

    function parseDetailRoute(href = pageWindow.location.href) {
        let url;
        try {
            url = new URL(href);
        } catch {
            return null;
        }
        if (!MTEAM.isHost(url.hostname.toLowerCase())) {
            return null;
        }
        const match = /^\/detail\/([0-9]+)\/?$/.exec(url.pathname);
        if (!match) {
            return null;
        }
        return { id: match[1], routeHref: url.href };
    }

    function isDetailRoutePath(href = pageWindow.location.href) {
        let url;
        try {
            url = new URL(href);
        } catch {
            return false;
        }
        return MTEAM.isHost(url.hostname.toLowerCase()) && /^\/detail(?:\/|$)/.test(url.pathname);
    }

    const diagnostics = {
        scriptVersion: '0.2.0',
        hrefPattern: '/detail/<id>',
        routeMatched: false,
        routeIdPresent: false,
        providerRunning: false,
        bridgeReady: false,
        detailSeen: false,
        candidateSource: 'none',
        nativeFloatMountFound: false,
        mountMode: 'none',
        nativeDownloadSeen: false,
        actionMounted: false
    };

    function updateDiagnostics(patch) {
        Object.assign(diagnostics, patch);
    }

    function diagnosticSnapshot() {
        return {
            ...diagnostics,
            bound: Boolean(storage.getConnection()?.token)
        };
    }

    function makeError(kind, message, status) {
        const error = new Error(message);
        error.kind = kind;
        error.status = status;
        return error;
    }

    function isAbortError(error) {
        return error && (error.kind === 'aborted' || error.name === 'AbortError');
    }

    function normalizeBaseUrl(value) {
        if (typeof value !== 'string' || !value.trim()) {
            throw makeError('config', 'TorrentFS 地址无效。');
        }
        const raw = value.trim();
        let url;
        try {
            url = new URL(raw);
        } catch {
            throw makeError('config', 'TorrentFS 地址无效。');
        }
        if (url.protocol !== 'http:' && url.protocol !== 'https:') {
            throw makeError('config', 'TorrentFS 地址必须使用 HTTP 或 HTTPS。');
        }
        if (url.username || url.password || /^[a-z][a-z0-9+.-]*:\/\/[^/?#]*@/i.test(raw) || raw.includes('?') || raw.includes('#')) {
            throw makeError('config', '地址不能包含用户名、密码、query 或 fragment。');
        }
        const path = url.pathname.replace(/\/+/g, '/').replace(/\/$/, '');
        if (/\/(?:api\/v1)(?:\/|$)/i.test(path)) {
            throw makeError('config', '请输入 TorrentFS 服务根地址，不要填写具体 API 地址。');
        }
        url.pathname = path || '/';
        url.search = '';
        url.hash = '';
        return `${url.origin}${url.pathname === '/' ? '' : url.pathname}`;
    }

    function joinApiUrl(baseUrl, suffix) {
        const url = new URL(normalizeBaseUrl(baseUrl));
        const prefix = url.pathname === '/' ? '' : url.pathname.replace(/\/+$/, '');
        url.pathname = `${prefix}/${String(suffix).replace(/^\/+/, '')}`;
        url.search = '';
        url.hash = '';
        return url.href;
    }

    function isBaseTarget(targetUrl, baseUrl) {
        let target;
        let base;
        try {
            target = new URL(targetUrl);
            base = new URL(normalizeBaseUrl(baseUrl));
        } catch {
            return false;
        }
        if (target.protocol !== base.protocol || target.host !== base.host) {
            return false;
        }
        const prefix = base.pathname === '/' ? '' : base.pathname.replace(/\/+$/, '');
        return !prefix || target.pathname === prefix || target.pathname.indexOf(`${prefix}/`) === 0;
    }

    function getConnection() {
        let value;
        try {
            value = GM_getValue(STORAGE_KEY, null);
        } catch {
            return null;
        }
        if (typeof value === 'string') {
            try {
                value = JSON.parse(value);
            } catch {
                return null;
            }
        }
        if (!value || typeof value !== 'object' || typeof value.baseUrl !== 'string') {
            return null;
        }
        let baseUrl;
        try {
            baseUrl = normalizeBaseUrl(value.baseUrl);
        } catch {
            return null;
        }
        const connection = {
            version: 2,
            baseUrl,
            username: typeof value.username === 'string' ? value.username : '',
            token: typeof value.token === 'string' ? value.token : '',
            pairedAt: Number.isFinite(value.pairedAt) ? value.pairedAt : 0,
            expiresAt: Number.isFinite(value.expiresAt) ? value.expiresAt : 0,
            insecureHttp: new URL(baseUrl).protocol === 'http:'
        };
        if (value.version !== 2 || value.baseUrl !== connection.baseUrl || value.insecureHttp !== connection.insecureHttp) {
            try {
                GM_setValue(STORAGE_KEY, connection);
            } catch {
                return connection;
            }
        }
        return connection;
    }

    function saveConnection(connection) {
        const baseUrl = normalizeBaseUrl(connection.baseUrl);
        const value = {
            version: 2,
            baseUrl,
            username: typeof connection.username === 'string' ? connection.username : '',
            token: typeof connection.token === 'string' ? connection.token : '',
            pairedAt: Number.isFinite(connection.pairedAt) ? connection.pairedAt : Date.now(),
            expiresAt: Number.isFinite(connection.expiresAt) ? connection.expiresAt : 0,
            insecureHttp: new URL(baseUrl).protocol === 'http:'
        };
        GM_setValue(STORAGE_KEY, value);
        return value;
    }

    function clearConnection() {
        try {
            GM_deleteValue(STORAGE_KEY);
        } catch {
            return;
        }
    }

    function clearConnectionIfTokenMatches(token) {
        const connection = getConnection();
        if (connection && connection.token === token) {
            saveConnection({ ...connection, token: '', expiresAt: 0 });
        }
    }

    // storage
    const storage = {
        getConnection,
        saveConnection,
        clearConnection,
        clearConnectionIfTokenMatches,
        normalizeBaseUrl,
        joinApiUrl,
        isBaseTarget
    };

    function getGMRequest() {
        if (typeof GM_xmlhttpRequest === 'function') {
            return GM_xmlhttpRequest;
        }
        if (typeof GM !== 'undefined' && typeof GM.xmlHttpRequest === 'function') {
            return GM.xmlHttpRequest;
        }
        throw makeError('transport', '当前 userscript 管理器不支持跨域请求。');
    }

    function gmRequest(options) {
        return new Promise((resolve, reject) => {
            let request;
            let settled = false;
            const signal = options.signal;
            const cleanup = () => {
                if (signal) {
                    signal.removeEventListener('abort', abort);
                }
            };
            const finish = (callback, value) => {
                if (settled) {
                    return;
                }
                settled = true;
                cleanup();
                callback(value);
            };
            const abort = () => {
                try {
                    request?.abort();
                } catch {
                    // The request may already have completed; still settle the caller.
                }
                finish(reject, makeError('aborted', '请求已取消。'));
            };

            if (signal?.aborted) {
                reject(makeError('aborted', '请求已取消。'));
                return;
            }
            if (signal) {
                signal.addEventListener('abort', abort, { once: true });
            }

            try {
                request = getGMRequest()({
                    method: options.method,
                    url: options.url,
                    headers: options.headers,
                    data: options.data,
                    responseType: options.responseType,
                    anonymous: options.anonymous,
                    redirect: options.redirect,
                    onload: (response) => finish(resolve, response),
                    onerror: () => finish(reject, makeError('transport', '网络请求失败。')),
                    ontimeout: () => finish(reject, makeError('timeout', '请求超时。')),
                    onabort: () => finish(reject, makeError('aborted', '请求已取消。'))
                });
                if (settled) {
                    request?.abort();
                }
            } catch (error) {
                finish(reject, error instanceof Error ? error : makeError('transport', '网络请求失败。'));
            }
        });
    }

    function requestWithDeadline(options, externalSignal) {
        const controller = new AbortController();
        let deadlineExceeded = false;
        let externallyAborted = false;
        const abortExternal = () => {
            externallyAborted = true;
            controller.abort();
        };
        if (externalSignal) {
            if (externalSignal.aborted) {
                abortExternal();
            } else {
                externalSignal.addEventListener('abort', abortExternal, { once: true });
            }
        }
        const timer = pageWindow.setTimeout(() => {
            deadlineExceeded = true;
            controller.abort();
        }, REQUEST_TIMEOUT);
        const request = gmRequest({
            ...options,
            signal: controller.signal
        });
        return request.catch((error) => {
            if (deadlineExceeded) {
                throw makeError('timeout', '请求超时。');
            }
            if (externallyAborted && error.kind === 'aborted') {
                throw error;
            }
            throw error;
        }).finally(() => {
            pageWindow.clearTimeout(timer);
            externalSignal?.removeEventListener('abort', abortExternal);
        });
    }

    function responseHeader(response, name) {
        const headers = String(response.responseHeaders || '');
        const wanted = name.toLowerCase();
        for (const line of headers.split(/\r?\n/)) {
            const separator = line.indexOf(':');
            if (separator < 0) {
                continue;
            }
            if (line.slice(0, separator).trim().toLowerCase() === wanted) {
                return line.slice(separator + 1).trim();
            }
        }
        return '';
    }

    function responseJson(response) {
        try {
            return JSON.parse(response.responseText || '');
        } catch {
            throw makeError('protocol', '服务返回了无法识别的响应。');
        }
    }

    function statusMessage(status) {
        switch (status) {
            case 400:
                return '站点返回的种子文件无效。';
            case 404:
                return 'TorrentFS 地址或 API 路径错误。';
            case 409:
                return '任务正在删除，请稍后重试。';
            case 413:
                return '种子文件超过 TorrentFS 上传限制。';
            case 415:
                return 'TorrentFS 不接受当前上传格式。';
            case 500:
                return 'TorrentFS 添加任务失败，请查看服务日志。';
            default:
                return `TorrentFS 请求失败（HTTP ${status}）。`;
        }
    }

    function safeTorrentFilename(id) {
        const safeId = String(id || 'torrent').replace(/[^a-zA-Z0-9_-]/g, '').slice(0, 80) || 'torrent';
        return `mteam-${safeId}.torrent`;
    }

    function assertResponseTarget(response, baseUrl) {
        if (response.status >= 300 && response.status < 400) {
            throw makeError('redirect', '服务返回重定向，出于凭据安全已拒绝。', response.status);
        }
        if (response.finalUrl && !storage.isBaseTarget(response.finalUrl, baseUrl)) {
            throw makeError('redirect', '请求被重定向到配置地址之外，出于凭据安全已拒绝。');
        }
    }

    // torrentfsClient
    const torrentfsClient = {
        async login(credentials, signal) {
            const baseUrl = storage.normalizeBaseUrl(credentials.baseUrl);
            const username = typeof credentials.username === 'string' ? credentials.username.trim() : '';
            const password = typeof credentials.password === 'string' ? credentials.password : '';
            if (!username) {
                throw makeError('config', '请输入 TorrentFS 用户名。');
            }
            if (!password) {
                throw makeError('config', '请输入 TorrentFS 密码。');
            }
            let response;
            try {
                response = await requestWithDeadline({
                    method: 'POST',
                    url: storage.joinApiUrl(baseUrl, '/api/v1/auth/login'),
                    headers: {
                        Accept: 'application/json',
                        'Cache-Control': 'no-store',
                        'Content-Type': 'application/json'
                    },
                    data: JSON.stringify({ username, password }),
                    anonymous: true,
                    redirect: 'error'
                }, signal);
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                if (error.kind === 'timeout') {
                    throw makeError('connection', '无法连接 TorrentFS，请检查地址和网络。');
                }
                throw makeError('connection', '无法连接 TorrentFS，请检查地址和网络。');
            }
            assertResponseTarget(response, baseUrl);
            if (response.status === 401) {
                throw makeError('login', '用户名或密码错误。', 401);
            }
            if (response.status === 404) {
                throw makeError('login', 'TorrentFS 地址或路径错误，或目标未启用 auth。', 404);
            }
            if (response.status === 413) {
                throw makeError('login', '登录输入超过服务限制。', 413);
            }
            if (response.status >= 500) {
                throw makeError('login', 'TorrentFS 登录失败，请查看服务日志。', response.status);
            }
            if (response.status !== 200) {
                throw makeError('login', '登录请求与 TorrentFS API 不兼容。', response.status);
            }
            const body = responseJson(response);
            const expiresIn = Number(body && body.expires_in);
            if (!body || typeof body.token !== 'string' || !body.token || typeof body.token_type !== 'string' || body.token_type.toLowerCase() !== 'bearer' || !Number.isFinite(expiresIn) || expiresIn <= 0) {
                throw makeError('protocol', 'TorrentFS 登录响应无效。');
            }
            return {
                baseUrl,
                username,
                token: body.token,
                pairedAt: Date.now(),
                expiresAt: Date.now() + expiresIn * 1000
            };
        },

        async verifyToken(connection, signal) {
            let response;
            try {
                response = await requestWithDeadline({
                    method: 'GET',
                    url: storage.joinApiUrl(connection.baseUrl, '/api/v1/torrents'),
                    headers: {
                        Accept: 'application/json',
                        Authorization: `Bearer ${connection.token}`
                    },
                    anonymous: true,
                    redirect: 'error'
                }, signal);
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                if (error.kind === 'timeout') {
                    throw makeError('connection', '无法连接 TorrentFS，请检查地址和网络。');
                }
                throw makeError('connection', '无法连接 TorrentFS，请检查地址和网络。');
            }
            assertResponseTarget(response, connection.baseUrl);
            if (response.status === 401) {
                throw makeError('unauthorized', 'TorrentFS 会话已失效。', 401);
            }
            if (response.status === 404) {
                throw makeError('login', 'TorrentFS 地址或路径错误，或目标未启用 auth。', 404);
            }
            if (response.status >= 500) {
                throw makeError('connection', 'TorrentFS 服务端失败，请稍后重试。', response.status);
            }
            if (response.status < 200 || response.status >= 300) {
                throw makeError('connection', `TorrentFS 连接检查失败（HTTP ${response.status}）。`, response.status);
            }
        },

        async logout(connection, signal) {
            if (!connection || !connection.token) {
                return;
            }
            let response;
            try {
                response = await requestWithDeadline({
                    method: 'POST',
                    url: storage.joinApiUrl(connection.baseUrl, '/api/v1/auth/logout'),
                    headers: {
                        Accept: 'application/json',
                        Authorization: `Bearer ${connection.token}`
                    },
                    anonymous: true,
                    redirect: 'error'
                }, signal);
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                throw makeError('connection', '无法连接 TorrentFS 完成解绑。');
            }
            assertResponseTarget(response, connection.baseUrl);
            if (response.status !== 204 && response.status !== 401) {
                throw makeError('connection', `TorrentFS 解绑失败（HTTP ${response.status}）。`, response.status);
            }
        },

        async bind(credentials, signal) {
            const baseUrl = storage.normalizeBaseUrl(credentials.baseUrl);
            if (new URL(baseUrl).protocol === 'http:' && credentials.httpConsent !== true) {
                throw makeError('insecure-consent', '请先确认 HTTP 明文传输风险。');
            }
            const previous = storage.getConnection();
            const loggedIn = await this.login({ ...credentials, baseUrl }, signal);
            try {
                await this.verifyToken(loggedIn, signal);
            } catch (error) {
                await this.logout(loggedIn).catch(() => {});
                throw error;
            }
            const saved = storage.saveConnection(loggedIn);
            if (previous && previous.token && (previous.baseUrl !== saved.baseUrl || previous.token !== saved.token)) {
                this.logout(previous).catch(() => {});
            }
            return saved;
        },

        async downloadTorrent(url, signal) {
            let parsed;
            try {
                parsed = new URL(url, pageWindow.location.href);
            } catch {
                throw makeError('mteam-download', 'M-Team 下载地址无效。');
            }
            if (!MTEAM.isHost(parsed.hostname.toLowerCase())) {
                throw makeError('mteam-download', 'M-Team 下载地址不在允许的站点范围内。');
            }
            let response;
            try {
                response = await requestWithDeadline({
                    method: 'GET',
                    url: parsed.href,
                    responseType: 'arraybuffer',
                    anonymous: false,
                    redirect: 'error'
                }, signal);
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                if (error.kind === 'timeout') {
                    throw makeError('mteam-download', 'M-Team 下载超时，请重试。');
                }
                throw makeError('mteam-download', 'M-Team 种子下载失败，请刷新详情页或重新登录。');
            }
            if (response.status >= 300 && response.status < 400) {
                throw makeError('mteam-download', 'M-Team 下载发生重定向，未自动跟随。');
            }
            if (response.finalUrl) {
                let finalUrl;
                try {
                    finalUrl = new URL(response.finalUrl);
                } catch {
                    throw makeError('mteam-download', 'M-Team 下载地址无效。');
                }
                if (!MTEAM.isHost(finalUrl.hostname.toLowerCase())) {
                    throw makeError('mteam-download', 'M-Team 下载重定向到不允许的地址。');
                }
            }
            if (response.status < 200 || response.status >= 300) {
                throw makeError('mteam-download', 'M-Team 种子下载失败，请刷新详情页或重新登录。', response.status);
            }
            if (responseHeader(response, 'content-type').toLowerCase().includes('text/html')) {
                throw makeError('mteam-download', 'M-Team 返回了登录页面，请重新登录后重试。');
            }
            const bytes = response.response;
            if (!bytes || typeof bytes.byteLength !== 'number' || bytes.byteLength === 0) {
                throw makeError('mteam-download', 'M-Team 返回了空的种子文件。');
            }
            if (bytes.byteLength > MAX_TORRENT_BYTES) {
                throw makeError('mteam-download', '种子文件超过本地安全大小限制。');
            }
            return bytes;
        },

        async upload(connection, bytes, filename, signal) {
            if (!connection || !connection.token) {
                throw makeError('unauthorized', '尚未绑定 TorrentFS。');
            }
            const baseUrl = storage.normalizeBaseUrl(connection.baseUrl);
            const token = connection.token;
            const form = new FormData();
            form.append('file', new Blob([bytes], { type: 'application/x-bittorrent' }), filename);
            let response;
            try {
                response = await requestWithDeadline({
                    method: 'POST',
                    url: storage.joinApiUrl(baseUrl, '/api/v1/torrents'),
                    headers: {
                        Accept: 'application/json',
                        Authorization: `Bearer ${token}`
                    },
                    data: form,
                    anonymous: true,
                    redirect: 'error'
                }, signal);
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                throw makeError('upload-unknown', error.kind === 'timeout' ? '上传结果未知，可重试。' : '上传结果未知，可重试。');
            }
            assertResponseTarget(response, baseUrl);
            if (response.status === 401) {
                storage.clearConnectionIfTokenMatches(token);
                throw makeError('unauthorized', 'TorrentFS 会话已失效，请重新绑定。', 401);
            }
            if (response.status !== 201) {
                throw makeError('upload', statusMessage(response.status), response.status);
            }
            return responseJson(response);
        }
    };

    // page bridge
    function pageBridgeScript(detailPath, downloadTokenPath, source) {
        const post = (payload) => window.postMessage(Object.assign({ source }, payload), '*');
        const installedKey = '__torrentfsMteamBridgeInstalled';
        if (window[installedKey]) {
            post({ type: 'bridge-ready', version: 1 });
            return;
        }
        window[installedKey] = true;
        const isDetailRequest = (url) => String(url || '').indexOf(detailPath) !== -1;
        const emitDetail = (text, routeHref) => {
            let response;
            try {
                response = JSON.parse(text);
            } catch {
                return;
            }
            if (!routeHref || !response || response.message !== 'SUCCESS' || !response.data || !response.data.id) {
                return;
            }
            post({
                type: 'detail',
                routeHref,
                candidate: {
                    id: response.data.id,
                    name: response.data.name,
                    originFileName: response.data.originFileName,
                    smallDescr: response.data.smallDescr
                }
            });
        };

        const originalOpen = XMLHttpRequest.prototype.open;
        XMLHttpRequest.prototype.open = function (method, url) {
            const requestUrl = String(url || '');
            const requestRouteHref = window.location.href;
            if (isDetailRequest(requestUrl) && !this.__torrentfsMteamDetailListener) {
                this.__torrentfsMteamDetailListener = true;
                this.addEventListener('readystatechange', () => {
                    if (this.readyState === 4 && this.status >= 200 && this.status < 300) {
                        emitDetail(this.responseText, requestRouteHref);
                    }
                });
            }
            return originalOpen.apply(this, arguments);
        };

        if (typeof window.fetch === 'function') {
            const originalFetch = window.fetch;
            window.fetch = function (input) {
                const requestUrl = typeof input === 'string' ? input : input && input.url;
                const requestRouteHref = window.location.href;
                const result = originalFetch.apply(this, arguments);
                if (!isDetailRequest(requestUrl)) {
                    return result;
                }
                return result.then((response) => {
                    try {
                        response.clone().text().then((text) => emitDetail(text, requestRouteHref)).catch(() => {});
                    } catch {
                        return response;
                    }
                    return response;
                });
            };
        }

        const notifyRouteChange = () => post({ type: 'route-change', routeHref: window.location.href });
        const originalPushState = window.history.pushState;
        const originalReplaceState = window.history.replaceState;
        window.history.pushState = function () {
            const result = originalPushState.apply(this, arguments);
            notifyRouteChange();
            return result;
        };
        window.history.replaceState = function () {
            const result = originalReplaceState.apply(this, arguments);
            notifyRouteChange();
            return result;
        };
        window.addEventListener('popstate', notifyRouteChange);
        window.addEventListener('hashchange', notifyRouteChange);

        window.addEventListener('message', (event) => {
            if (event.source !== window || !event.data || event.data.source !== source || event.data.type !== 'request-download-url') {
                return;
            }
            const requestId = String(event.data.requestId || '');
            const torrentId = String(event.data.torrentId || '');
            if (!requestId || !torrentId) {
                return;
            }
            const apiHost = localStorage.getItem('apiHost') || window.location.origin;
            const auth = localStorage.getItem('auth') || '';
            if (!auth) {
                post({ type: 'download-url-error', requestId, reason: 'missing-session' });
                return;
            }
            fetch(`${apiHost.replace(/\/+$/, '')}${downloadTokenPath}`, {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8',
                    TS: String(Math.floor(Date.now() / 1000)),
                    Authorization: auth
                },
                body: new URLSearchParams({ id: torrentId })
            }).then((response) => {
                if (!response.ok) {
                    throw new Error('http');
                }
                return response.json();
            }).then((response) => {
                if (!response || response.code !== '0' || typeof response.data !== 'string' || !response.data) {
                    throw new Error('business');
                }
                post({ type: 'download-url', requestId, url: response.data });
            }).catch(() => {
                post({ type: 'download-url-error', requestId, reason: 'request-failed' });
            });
        });
        post({ type: 'bridge-ready', version: 1 });
    }

    const pageBridge = (() => {
        const MAX_ATTEMPTS = 3;
        const BRIDGE_TIMEOUT = 1200;
        let bridgeReady = false;
        let bridgeFailed = false;
        let injecting = false;
        let attempts = 0;
        let retryTimer;
        let readyTimer;
        let requestSequence = 0;
        const stateListeners = new Set();
        const notifyState = (state) => stateListeners.forEach((listener) => listener(state));

        const isBridgeMessage = (event) => (event.source === pageWindow || event.source === window) && event.data && event.data.source === BRIDGE_SOURCE;
        const bridgeListener = (event) => {
            if (!isBridgeMessage(event) || event.data.type !== 'bridge-ready' || event.data.version !== 1) {
                return;
            }
            bridgeReady = true;
            bridgeFailed = false;
            injecting = false;
            pageWindow.clearTimeout(readyTimer);
            pageWindow.clearTimeout(retryTimer);
            updateDiagnostics({ bridgeReady: true });
            notifyState('ready');
        };
        pageWindow.addEventListener('message', bridgeListener);

        function injectScript(text) {
            const root = document.documentElement || document.head || document.body;
            if (!root) {
                return false;
            }
            if (typeof GM_addElement === 'function') {
                try {
                    GM_addElement(root, 'script', { textContent: text });
                    return true;
                } catch {
                    // Fall through to the DOM injection path.
                }
            }
            try {
                const script = document.createElement('script');
                script.textContent = text;
                root.appendChild(script);
                script.remove();
                return true;
            } catch {
                return false;
            }
        }

        function attemptInstall() {
            if (bridgeReady || bridgeFailed || injecting) {
                return;
            }
            if (attempts >= MAX_ATTEMPTS) {
                bridgeFailed = true;
                updateDiagnostics({ bridgeReady: false });
                notifyState('failed');
                return;
            }
            attempts += 1;
            injecting = true;
            const script = `(${pageBridgeScript.toString()})(${JSON.stringify(MTEAM.detailPath)},${JSON.stringify(MTEAM.downloadTokenPath)},${JSON.stringify(BRIDGE_SOURCE)});`;
            if (!injectScript(script)) {
                injecting = false;
            }
            readyTimer = pageWindow.setTimeout(() => {
                if (bridgeReady) {
                    return;
                }
                injecting = false;
                if (attempts < MAX_ATTEMPTS) {
                    retryTimer = pageWindow.setTimeout(attemptInstall, 250);
                } else {
                    bridgeFailed = true;
                    updateDiagnostics({ bridgeReady: false });
                    notifyState('failed');
                }
            }, BRIDGE_TIMEOUT);
        }

        function install() {
            if (!bridgeReady && !bridgeFailed) {
                attemptInstall();
            }
        }

        function listenDetail(callback) {
            const listener = (event) => {
                if (!isBridgeMessage(event) || event.data.type !== 'detail') {
                    return;
                }
                callback(event.data);
            };
            pageWindow.addEventListener('message', listener);
            return () => pageWindow.removeEventListener('message', listener);
        }

        function listenRoute(callback) {
            const listener = (event) => {
                if (!isBridgeMessage(event) || event.data.type !== 'route-change') {
                    return;
                }
                callback(event.data.routeHref);
            };
            pageWindow.addEventListener('message', listener);
            return () => pageWindow.removeEventListener('message', listener);
        }

        function requestDownloadUrl(torrentId, signal) {
            return new Promise((resolve, reject) => {
                if (bridgeFailed) {
                    reject(makeError('bridge-unavailable', '页面桥接未就绪，无法获取 M-Team 下载地址。'));
                    return;
                }
                if (!bridgeReady) {
                    install();
                    reject(makeError('bridge-unavailable', '页面桥接尚未就绪，无法获取 M-Team 下载地址。'));
                    return;
                }
                const requestId = `${Date.now()}-${++requestSequence}`;
                let settled = false;
                const timer = pageWindow.setTimeout(() => finish(reject, makeError('mteam-token-timeout', 'M-Team 下载地址请求超时。')), REQUEST_TIMEOUT);
                const cleanup = () => {
                    pageWindow.clearTimeout(timer);
                    pageWindow.removeEventListener('message', listener);
                    signal?.removeEventListener('abort', abort);
                };
                const finish = (callback, value) => {
                    if (settled) {
                        return;
                    }
                    settled = true;
                    cleanup();
                    callback(value);
                };
                const abort = () => finish(reject, makeError('aborted', '请求已取消。'));
                const listener = (event) => {
                    if (!isBridgeMessage(event) || event.data.requestId !== requestId) {
                        return;
                    }
                    if (event.data.type === 'download-url') {
                        finish(resolve, event.data.url);
                    } else if (event.data.type === 'download-url-error') {
                        finish(reject, makeError('mteam-token', 'M-Team 下载地址获取失败。'));
                    }
                };
                if (signal?.aborted) {
                    finish(reject, makeError('aborted', '请求已取消。'));
                    return;
                }
                pageWindow.addEventListener('message', listener);
                signal?.addEventListener('abort', abort, { once: true });
                pageWindow.postMessage({
                    source: BRIDGE_SOURCE,
                    type: 'request-download-url',
                    requestId,
                    torrentId: String(torrentId)
                }, '*');
            });
        }

        function listenState(callback) {
            stateListeners.add(callback);
            return () => stateListeners.delete(callback);
        }

        return {
            install,
            listenDetail,
            listenRoute,
            listenState,
            requestDownloadUrl,
            isReady: () => bridgeReady,
            isFailed: () => bridgeFailed
        };
    })();

    // UI
    const ui = (() => {
        let stylesAdded = false;
        let statusElement;

        function ensureStyles() {
            if (stylesAdded) {
                return;
            }
            stylesAdded = true;
            const css = `
                .torrentfs-mteam-button { align-items: center; background: #1677ff; border: 0; border-radius: 50%; box-shadow: 0 4px 12px rgba(0,0,0,.24); color: #fff; cursor: pointer; display: inline-flex; font: 700 12px/1 sans-serif; height: 44px; justify-content: center; margin: 0; padding: 0; width: 44px; }
                .torrentfs-mteam-button:hover { background: #4096ff; }
                .torrentfs-mteam-button:focus-visible { outline: 3px solid rgba(22,119,255,.35); outline-offset: 2px; }
                .torrentfs-mteam-button:disabled { cursor: not-allowed; opacity: .65; }
                #torrentfs-float-root { bottom: 96px; position: fixed; right: 24px; z-index: 2147483646; }
                #torrentfs-mteam-status { background: #fff; border: 1px solid #d9d9d9; border-radius: 6px; bottom: 48px; box-shadow: 0 4px 12px rgba(0,0,0,.15); color: #262626; display: none; font: 14px/1.4 sans-serif; max-width: 280px; padding: 10px 12px; position: fixed; right: 84px; z-index: 2147483646; }
                #torrentfs-mteam-status[data-state="error"] { border-color: #ff4d4f; color: #cf1322; }
                #torrentfs-mteam-status[data-state="success"] { border-color: #52c41a; color: #389e0d; }
                #torrentfs-mteam-status[data-state="busy"] { border-color: #1677ff; color: #0958d9; }
            `;
            if (typeof GM_addStyle === 'function') {
                GM_addStyle(css);
            } else {
                const style = document.createElement('style');
                style.textContent = css;
                (document.head || document.documentElement).appendChild(style);
            }
        }

        function setStatus(state, message) {
            ensureStyles();
            if (!statusElement) {
                statusElement = document.createElement('div');
                statusElement.id = 'torrentfs-mteam-status';
                (document.body || document.documentElement).appendChild(statusElement);
            }
            statusElement.dataset.state = state;
            statusElement.textContent = message;
            statusElement.style.display = 'block';
        }

        function clearStatus() {
            if (statusElement) {
                statusElement.textContent = '';
                statusElement.style.display = 'none';
            }
        }

        function openCredentialForm(initialConnection, onSubmit, onCancel, onClose) {
            const promptFn = sandboxPrompt;
            const confirmFn = sandboxConfirm;
            if (!promptFn || !confirmFn) {
                setStatus('error', '当前浏览器不支持安全配置界面。');
                return { close() {} };
            }
            let closed = false;
            const close = () => {
                if (closed) {
                    return;
                }
                closed = true;
                onClose?.();
            };
            const cancel = () => {
                onCancel?.();
                close();
            };
            const baseUrl = promptFn('TorrentFS 服务地址（HTTP 或 HTTPS）', initialConnection?.baseUrl || '');
            if (baseUrl === null) {
                cancel();
                return { close };
            }
            let isHttp = false;
            try {
                isHttp = new URL(baseUrl.trim()).protocol === 'http:';
            } catch {
                // The client reports invalid addresses without sending a request.
            }
            if (isHttp && !confirmFn('当前 TorrentFS 地址使用 HTTP。用户名、密码、Bearer token、torrent 元数据及上传内容可能被观察、窃取或篡改。继续表示你理解并自行承担该风险。')) {
                cancel();
                return { close };
            }
            const username = promptFn('TorrentFS 用户名', initialConnection?.username || '');
            if (username === null) {
                cancel();
                return { close };
            }
            const password = promptFn('TorrentFS 密码（仅本次登录使用，不会保存）', '');
            if (password === null) {
                cancel();
                return { close };
            }
            const credentials = {
                baseUrl,
                username,
                password,
                httpConsent: isHttp
            };
            const sendResult = (result) => {
                if (closed) {
                    return;
                }
                setStatus(result.ok === true ? 'success' : 'error', result.message || (result.ok ? '绑定成功。' : '绑定失败，请重试。'));
                close();
            };
            Promise.resolve().then(() => onSubmit(credentials)).then((result) => {
                credentials.password = '';
                if (!closed) {
                    sendResult(result || { ok: false, message: '绑定失败，请重试。' });
                }
            }).catch(() => {
                credentials.password = '';
                if (!closed) {
                    sendResult({ ok: false, message: '绑定失败，请重试。' });
                }
            });
            return { close };
        }

        function createAction(onClick, mode) {
            ensureStyles();
            const element = document.createElement('button');
            element.type = 'button';
            element.id = 'torrentfs-mteam-float-action';
            element.className = 'torrentfs-mteam-button';
            element.dataset.mode = mode;
            element.title = '发送到 TorrentFS';
            element.setAttribute('aria-label', '发送到 TorrentFS');
            element.textContent = 'TF';
            const listener = (event) => {
                event.preventDefault();
                onClick();
            };
            element.addEventListener('click', listener);
            return {
                element,
                setBusy(busy) {
                    element.disabled = busy;
                    element.textContent = busy ? '…' : element.dataset.label || 'TF';
                },
                setLabel(label) {
                    element.dataset.label = label;
                    element.title = '发送到 TorrentFS';
                    element.setAttribute('aria-label', '发送到 TorrentFS');
                    if (!element.disabled) {
                        element.textContent = label === '发送到 TorrentFS' ? 'TF' : label === '正在识别种子…' ? '…' : '×';
                    }
                },
                setMode(mode) {
                    element.dataset.mode = mode;
                },
                setDisabled(disabled) {
                    element.disabled = disabled;
                },
                dispose() {
                    element.removeEventListener('click', listener);
                    element.remove();
                }
            };
        }

        return { ensureStyles, setStatus, clearStatus, createAction, openCredentialForm };
    })();

    function connectionStatus(connection) {
        if (!connection) {
            return 'TorrentFS 未绑定，请先配置连接。';
        }
        if (!connection.token) {
            return connection.insecureHttp ? 'TorrentFS 未绑定（HTTP 不安全），请重新绑定。' : 'TorrentFS 未绑定，请重新绑定。';
        }
        return connection.insecureHttp ? 'TorrentFS 已绑定（HTTP 不安全）。' : 'TorrentFS 已绑定（HTTPS）。';
    }

    function userMessage(error) {
        if (!error) {
            return '提交失败，请重试。';
        }
        if (['config', 'insecure-consent', 'login', 'connection', 'redirect', 'protocol'].includes(error.kind)) {
            return error.message;
        }
        if (error.kind === 'unauthorized') {
            return 'TorrentFS 会话已失效，请重新绑定。';
        }
        if (error.kind === 'mteam-token' || error.kind === 'mteam-token-timeout') {
            return error.kind === 'mteam-token-timeout' ? 'M-Team 下载地址请求超时，请重试。' : 'M-Team 下载地址获取失败，请刷新详情页或重新登录。';
        }
        if (error.kind === 'bridge-unavailable') {
            return '页面桥接未就绪，请刷新详情页后重试。';
        }
        if (error.kind === 'mteam-download' || error.kind === 'upload' || error.kind === 'upload-unknown') {
            return error.message;
        }
        return '提交失败，请重试。';
    }

    // mteamProvider
    const mteamProvider = {
        matches() {
            return MTEAM.matchesPage();
        },

        start(routeCandidate = parseDetailRoute() || (isDetailRoutePath() ? { id: '', routeHref: pageWindow.location.href, invalid: true } : null)) {
            if (!routeCandidate) {
                return;
            }
            ui.ensureStyles();
            document.querySelectorAll('.torrentfs-mteam-button').forEach((element) => element.remove());
            pageBridge.install();

            let candidate = {
                ...routeCandidate,
                source: 'route',
                name: '',
                originFileName: '',
                smallDescr: '',
                conflict: false,
                invalid: routeCandidate.invalid === true || !routeCandidate.id
            };
            let candidateRouteHref = routeCandidate.routeHref;
            let routeKey = `${routeCandidate.routeHref}:${routeCandidate.id}`;
            let action;
            let activeController;
            let credentialSession;
            let bindingController;
            let lastHref = pageWindow.location.href;
            let mountTimer;
            let fallbackRoot;
            updateDiagnostics({
                routeMatched: true,
                routeIdPresent: Boolean(routeCandidate.id),
                providerRunning: true,
                detailSeen: false,
                candidateSource: 'route',
                nativeFloatMountFound: Boolean(document.querySelector(MTEAM.selectors.floatMount)),
                mountMode: 'none',
                nativeDownloadSeen: false,
                actionMounted: false,
                bridgeReady: pageBridge.isReady()
            });

            const disposeAction = () => {
                activeController?.abort();
                activeController = undefined;
                action?.dispose();
                action = undefined;
            };

            const scheduleMount = () => {
                if (mountTimer !== undefined) {
                    return;
                }
                mountTimer = pageWindow.setTimeout(() => {
                    mountTimer = undefined;
                    mountAction();
                }, 0);
            };

            const bridgeStateDispose = pageBridge.listenState((state) => {
                updateDiagnostics({ bridgeReady: state === 'ready' });
                if (state === 'failed' && action && !candidate?.conflict) {
                    action.setLabel('页面桥接未就绪');
                    ui.setStatus('error', '页面桥接未就绪，请刷新详情页后重试。');
                }
                scheduleMount();
            });

            const findFloatMount = () => {
                if (fallbackRoot && !document.contains(fallbackRoot)) {
                    fallbackRoot = undefined;
                }
                const nativeMount = document.querySelector(MTEAM.selectors.floatMount);
                const nativeDownloadSeen = Array.from(document.querySelectorAll(MTEAM.selectors.nativeDownload)).some((button) => button.textContent?.trim() === '下載');
                updateDiagnostics({ nativeFloatMountFound: Boolean(nativeMount), nativeDownloadSeen });
                if (nativeMount) {
                    if (fallbackRoot) {
                        fallbackRoot.remove();
                        fallbackRoot = undefined;
                    }
                    return { element: nativeMount, mode: 'native-float' };
                }
                if (!fallbackRoot && document.body) {
                    fallbackRoot = document.createElement('div');
                    fallbackRoot.id = 'torrentfs-float-root';
                    document.body.appendChild(fallbackRoot);
                }
                return fallbackRoot ? { element: fallbackRoot, mode: 'fallback-float' } : null;
            };

            const closeCredentialSession = () => {
                bindingController?.abort();
                bindingController = undefined;
                credentialSession?.close();
                credentialSession = undefined;
            };

            const openConfig = (resumeCandidate) => {
                closeCredentialSession();
                const expectedRouteHref = pageWindow.location.href;
                const resume = resumeCandidate ? { id: resumeCandidate.id } : undefined;
                credentialSession = ui.openCredentialForm(storage.getConnection(), async (credentials) => {
                    bindingController = new AbortController();
                    try {
                        const connection = await torrentfsClient.bind(credentials, bindingController.signal);
                        ui.setStatus('success', connectionStatus(connection));
                        if (resume && pageWindow.location.href === expectedRouteHref && candidateRouteHref === expectedRouteHref && candidate?.id === resume.id) {
                            pageWindow.setTimeout(() => {
                                if (pageWindow.location.href === expectedRouteHref && candidateRouteHref === expectedRouteHref && candidate?.id === resume.id && action) {
                                    submit(candidate, action);
                                }
                            }, 650);
                        }
                        return { ok: true, message: 'TorrentFS 绑定成功。' };
                    } catch (error) {
                        if (isAbortError(error)) {
                            return { ok: false, message: '绑定已取消。' };
                        }
                        return { ok: false, message: userMessage(error) };
                    } finally {
                        bindingController = undefined;
                    }
                }, () => {
                    bindingController?.abort();
                }, () => {
                    credentialSession = undefined;
                });
                return credentialSession;
            };

            const unpair = async () => {
                const connection = storage.getConnection();
                try {
                    await torrentfsClient.logout(connection);
                } catch {
                    // Local cleanup still wins when the daemon cannot revoke the token.
                } finally {
                    storage.clearConnection();
                    ui.setStatus('unbound', 'TorrentFS 绑定已解除。');
                }
            };

            registerPairingCommands(openConfig, unpair);

            const submit = async (selectedCandidate, selectedAction) => {
                if (activeController) {
                    return;
                }
                if (!selectedCandidate || selectedCandidate.invalid || selectedCandidate.conflict || !selectedCandidate.id) {
                    ui.setStatus('error', '无法识别当前种子，请刷新详情页。');
                    return;
                }
                const connection = storage.getConnection();
                if (!connection || !connection.token) {
                    ui.setStatus('unbound', connectionStatus(connection));
                    openConfig(selectedCandidate);
                    return;
                }
                const controller = new AbortController();
                activeController = controller;
                selectedAction.setBusy(true);
                ui.setStatus('busy', '正在获取 M-Team 种子并提交到 TorrentFS…');
                try {
                    const downloadUrl = await pageBridge.requestDownloadUrl(selectedCandidate.id, controller.signal);
                    const bytes = await torrentfsClient.downloadTorrent(downloadUrl, controller.signal);
                    const response = await torrentfsClient.upload(connection, bytes, safeTorrentFilename(selectedCandidate.id), controller.signal);
                    const name = typeof response.name === 'string' && response.name ? response.name : selectedCandidate.name || 'torrent';
                    const infoHash = typeof response.info_hash === 'string' ? response.info_hash : '';
                    const suffix = infoHash ? `（${infoHash.slice(0, 8)}）` : '';
                    ui.setStatus('success', `任务已可用：${name}${suffix}`);
                } catch (error) {
                    if (!isAbortError(error) && !controller.signal.aborted) {
                        ui.setStatus('error', userMessage(error));
                        if (error.kind === 'unauthorized') {
                            openConfig(selectedCandidate);
                        }
                    }
                } finally {
                    if (activeController === controller) {
                        activeController = undefined;
                    }
                    if (action === selectedAction) {
                        selectedAction.setBusy(false);
                    }
                }
            };

            const mountAction = () => {
                const mount = findFloatMount();
                if (!mount) {
                    return;
                }
                document.querySelectorAll('#torrentfs-mteam-float-action').forEach((element) => {
                    if (!action || element !== action.element) {
                        element.remove();
                    }
                });
                if (!action) {
                    action = ui.createAction(() => submit(candidate, action), mount.mode);
                    mount.element.appendChild(action.element);
                } else if (action.element.parentElement !== mount.element) {
                    mount.element.appendChild(action.element);
                    action.setMode(mount.mode);
                }
                updateDiagnostics({ actionMounted: true, mountMode: mount.mode });
                const connection = storage.getConnection();
                if (candidate?.invalid) {
                    action.setLabel('无法识别当前种子');
                    action.setDisabled(true);
                    ui.setStatus('error', '无法识别详情 ID。');
                } else if (candidate?.conflict) {
                    action.setLabel('无法识别当前种子');
                    action.setDisabled(true);
                    ui.setStatus('error', '详情 ID 与 URL 不一致，请刷新详情页。');
                } else if (candidate?.source === 'route' && !candidate?.name) {
                    action.setLabel(pageBridge.isFailed() ? '页面桥接未就绪' : '正在识别种子…');
                    action.setDisabled(false);
                    ui.setStatus(pageBridge.isFailed() ? 'error' : 'busy', pageBridge.isFailed() ? '页面桥接未就绪，入口仍可见。' : '正在识别种子…');
                } else {
                    action.setLabel('发送到 TorrentFS');
                    action.setDisabled(false);
                    ui.setStatus(connection?.token ? 'ready' : 'unbound', connectionStatus(connection));
                }
            };

            scheduleMount();

            const handleDetail = (value) => {
                const responseRouteHref = value && typeof value.routeHref === 'string' ? value.routeHref : '';
                const currentRouteHref = pageWindow.location.href;
                if (!responseRouteHref || responseRouteHref !== currentRouteHref || !value.candidate || value.candidate.id === undefined || value.candidate.id === null || !parseDetailRoute(currentRouteHref)) {
                    return;
                }
                updateDiagnostics({ detailSeen: true });
                const responseId = String(value.candidate.id);
                if (candidate && candidate.id !== responseId) {
                    candidate = { ...candidate, conflict: true, responseId };
                    updateDiagnostics({ candidateSource: 'conflict' });
                    scheduleMount();
                    return;
                }
                candidate = {
                    ...candidate,
                    id: responseId,
                    routeHref: responseRouteHref,
                    source: 'network',
                    name: typeof value.candidate.name === 'string' ? value.candidate.name : candidate?.name || '',
                    originFileName: typeof value.candidate.originFileName === 'string' ? value.candidate.originFileName : candidate?.originFileName || '',
                    smallDescr: typeof value.candidate.smallDescr === 'string' ? value.candidate.smallDescr : candidate?.smallDescr || '',
                    conflict: false
                };
                updateDiagnostics({ candidateSource: 'network' });
                lastHref = responseRouteHref;
                scheduleMount();
            };

            const detailDispose = pageBridge.listenDetail(handleDetail);
            const routeChanged = (observedHref) => {
                const currentRouteHref = pageWindow.location.href;
                if (observedHref && observedHref !== currentRouteHref) {
                    return;
                }
                if (lastHref === currentRouteHref) {
                    return;
                }
                lastHref = currentRouteHref;
                if (candidateRouteHref === currentRouteHref) {
                    return;
                }
                closeCredentialSession();
                disposeAction();
                candidate = undefined;
                candidateRouteHref = '';
                routeKey = '';
                ui.clearStatus();
            };
            pageWindow.addEventListener('popstate', routeChanged);
            pageWindow.addEventListener('hashchange', routeChanged);
            const routeDispose = pageBridge.listenRoute(routeChanged);

            const observer = new MutationObserver(() => {
                routeChanged();
                if (action && !document.contains(action.element)) {
                    updateDiagnostics({ actionMounted: false });
                    scheduleMount();
                }
                if (candidate) {
                    scheduleMount();
                }
            });
            if (document.documentElement) {
                observer.observe(document.documentElement, { childList: true, subtree: true });
            }

            return () => {
                detailDispose();
                bridgeStateDispose();
                routeDispose();
                pageWindow.removeEventListener('popstate', routeChanged);
                pageWindow.removeEventListener('hashchange', routeChanged);
                pageWindow.clearTimeout(mountTimer);
                observer.disconnect();
                closeCredentialSession();
                disposeAction();
                ui.clearStatus();
                fallbackRoot?.remove();
                fallbackRoot = undefined;
                updateDiagnostics({ providerRunning: false, actionMounted: false, mountMode: 'none', candidateSource: 'none', detailSeen: false, nativeFloatMountFound: false, nativeDownloadSeen: false });
            };
        }
    };

    const routeCoordinator = (() => {
        let providerCleanup;
        let routeKey = '';
        let routeDispose;
        let observer;
        let routeTimer;

        const syncRoute = () => {
            const validRoute = parseDetailRoute();
            const route = validRoute || (isDetailRoutePath() ? { id: '', routeHref: pageWindow.location.href, invalid: true } : null);
            const nextKey = route ? route.routeHref : '';
            updateDiagnostics({ routeMatched: Boolean(route), routeIdPresent: Boolean(validRoute?.id) });
            if (nextKey === routeKey) {
                return;
            }
            routeKey = nextKey;
            providerCleanup?.();
            providerCleanup = undefined;
            if (route) {
                providerCleanup = mteamProvider.start(route);
            }
        };

        function start() {
            routeDispose = pageBridge.listenRoute(syncRoute);
            pageWindow.addEventListener('popstate', syncRoute);
            pageWindow.addEventListener('hashchange', syncRoute);
            observer = new MutationObserver(syncRoute);
            if (document.documentElement) {
                observer.observe(document.documentElement, { childList: true, subtree: true });
            }
            routeTimer = pageWindow.setInterval(syncRoute, 500);
            syncRoute();
            pageBridge.install();
            return () => {
                routeDispose?.();
                pageWindow.removeEventListener('popstate', syncRoute);
                pageWindow.removeEventListener('hashchange', syncRoute);
                pageWindow.clearInterval(routeTimer);
                observer?.disconnect();
                providerCleanup?.();
                providerCleanup = undefined;
            };
        }

        return { start };
    })();

    const pairingMenu = { registered: false, openConfig: null, unpair: null };
    let diagnosticsMenuRegistered = false;

    function registerDiagnosticsMenu() {
        if (diagnosticsMenuRegistered || typeof GM_registerMenuCommand !== 'function') {
            return;
        }
        diagnosticsMenuRegistered = true;
        GM_registerMenuCommand('显示 TorrentFS 页面诊断', () => {
            sandboxPrompt?.('TorrentFS 页面诊断（可复制）', JSON.stringify(diagnosticSnapshot(), null, 2));
        });
    }

    function registerPairingCommands(openConfig, unpair) {
        pairingMenu.openConfig = openConfig;
        pairingMenu.unpair = unpair;
        if (pairingMenu.registered || typeof GM_registerMenuCommand !== 'function') {
            return;
        }
        pairingMenu.registered = true;
        GM_registerMenuCommand('配置 / 重新绑定 TorrentFS', () => pairingMenu.openConfig?.());
        GM_registerMenuCommand('解除 TorrentFS 绑定', () => { void pairingMenu.unpair?.(); });
        registerDiagnosticsMenu();
    }

    function bootstrap() {
        const host = pageWindow.location.hostname.toLowerCase();
        if (!MTEAM.isHost(host)) {
            return;
        }
        registerDiagnosticsMenu();
        routeCoordinator.start();
    }

    bootstrap();

})();
