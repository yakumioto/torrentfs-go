// ==UserScript==
// @name         TorrentFS M-Team bridge
// @namespace    https://github.com/yakumioto/torrentfs-go
// @version      0.2.0
// @description  Send a torrent from an M-Team detail page to a configured TorrentFS instance.
// @match        https://m-team.cc/detail/*
// @match        https://*.m-team.cc/detail/*
// @match        https://m-team.io/detail/*
// @match        https://*.m-team.io/detail/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_deleteValue
// @grant        GM_registerMenuCommand
// @grant        GM_addStyle
// @grant        unsafeWindow
// @connect      *
// @run-at       document-start
// @license      MPL-2.0
// ==/UserScript==

(function () {
    'use strict';

    const STORAGE_KEY = 'torrentfs.connection';
    const MAX_TORRENT_BYTES = 10 * 1024 * 1024;
    const REQUEST_TIMEOUT = 30 * 1000;
    const BRIDGE_SOURCE = 'torrentfs-mteam-bridge';
    const pageWindow = typeof unsafeWindow !== 'undefined' ? unsafeWindow : window;

    const MTEAM = {
        detailPath: '/api/torrent/detail',
        downloadTokenPath: '/torrent/genDlToken',
        hosts: ['m-team.cc', 'm-team.io'],
        selectors: {
            appContent: '.mt-4.app-content__inner',
            preferredMount: 'button.ant-btn.ant-btn-link.ant-btn-sm.ant-dropdown-trigger',
            fallbackMount: '.mt-4>div'
        },
        matchesPage() {
            const host = pageWindow.location.hostname.toLowerCase();
            const path = pageWindow.location.pathname;
            return this.isHost(host) && path.indexOf('/detail/') === 0;
        },
        isHost(host) {
            return this.hosts.some((domain) => host === domain || host.endsWith(`.${domain}`));
        }
    };

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
        const installedKey = '__torrentfsMteamBridgeInstalled';
        if (window[installedKey]) {
            return;
        }
        window[installedKey] = true;

        const post = (payload) => window.postMessage(Object.assign({ source }, payload), '*');
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
    }

    const pageBridge = (() => {
        let installed = false;
        let requestSequence = 0;

        function install() {
            if (installed) {
                return;
            }
            installed = true;
            const root = document.documentElement || document.head || document.body;
            if (!root) {
                installed = false;
                document.addEventListener('readystatechange', install, { once: true });
                return;
            }
            const script = document.createElement('script');
            script.textContent = `(${pageBridgeScript.toString()})(${JSON.stringify(MTEAM.detailPath)},${JSON.stringify(MTEAM.downloadTokenPath)},${JSON.stringify(BRIDGE_SOURCE)});`;
            root.appendChild(script);
            script.remove();
        }

        function listenDetail(callback) {
            const listener = (event) => {
                if ((event.source !== pageWindow && event.source !== window) || !event.data || event.data.source !== BRIDGE_SOURCE || event.data.type !== 'detail') {
                    return;
                }
                callback(event.data);
            };
            pageWindow.addEventListener('message', listener);
            return () => pageWindow.removeEventListener('message', listener);
        }

        function listenRoute(callback) {
            const listener = (event) => {
                if ((event.source !== pageWindow && event.source !== window) || !event.data || event.data.source !== BRIDGE_SOURCE || event.data.type !== 'route-change') {
                    return;
                }
                callback(event.data.routeHref);
            };
            pageWindow.addEventListener('message', listener);
            return () => pageWindow.removeEventListener('message', listener);
        }

        function requestDownloadUrl(torrentId, signal) {
            return new Promise((resolve, reject) => {
                const requestId = `${Date.now()}-${++requestSequence}`;
                let settled = false;
                const cleanup = () => {
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
                    if ((event.source !== pageWindow && event.source !== window) || !event.data || event.data.source !== BRIDGE_SOURCE || event.data.requestId !== requestId) {
                        return;
                    }
                    if (event.data.type === 'download-url') {
                        finish(resolve, event.data.url);
                    } else if (event.data.type === 'download-url-error') {
                        finish(reject, makeError('mteam-token', 'M-Team 下载地址获取失败。'));
                    }
                };
                if (signal?.aborted) {
                    reject(makeError('aborted', '请求已取消。'));
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

        return { install, listenDetail, listenRoute, requestDownloadUrl };
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
                .torrentfs-mteam-button { border: 1px solid #1677ff; border-radius: 4px; background: #1677ff; color: #fff; cursor: pointer; font: inherit; margin: 4px; padding: 6px 12px; }
                .torrentfs-mteam-button:hover { background: #4096ff; }
                .torrentfs-mteam-button:disabled { cursor: wait; opacity: .65; }
                .torrentfs-mteam-button[data-fixed="true"] { bottom: 24px; position: fixed; right: 24px; z-index: 2147483646; }
                #torrentfs-mteam-config-overlay { align-items: center; background: rgba(0,0,0,.45); display: flex; inset: 0; justify-content: center; padding: 16px; position: fixed; z-index: 2147483647; }
                #torrentfs-mteam-config-overlay iframe { background: Canvas; border: 0; border-radius: 8px; box-shadow: 0 10px 40px rgba(0,0,0,.35); height: min(560px, calc(100vh - 32px)); max-width: 520px; width: min(520px, 100%); }
                #torrentfs-mteam-status { background: #fff; border: 1px solid #d9d9d9; border-radius: 6px; bottom: 72px; box-shadow: 0 4px 12px rgba(0,0,0,.15); color: #262626; display: none; font: 14px/1.4 sans-serif; max-width: 360px; padding: 10px 12px; position: fixed; right: 24px; z-index: 2147483646; }
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

        function randomHex(byteLength) {
            const bytes = new Uint8Array(byteLength);
            if (globalThis.crypto?.getRandomValues) {
                globalThis.crypto.getRandomValues(bytes);
            } else {
                for (let index = 0; index < bytes.length; index += 1) {
                    bytes[index] = Math.floor(Math.random() * 256);
                }
            }
            return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
        }

        function randomNonce() {
            return randomHex(16);
        }

        function sha256Hex(input) {
            const constants = [
                0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
                0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
                0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
                0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
                0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
                0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
                0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
                0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
            ];
            const state = [0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19];
            const bytes = [];
            for (let index = 0; index < input.length; index += 1) bytes.push(input.charCodeAt(index) & 0xff);
            const bitLength = bytes.length * 8;
            bytes.push(0x80);
            while (bytes.length % 64 !== 56) bytes.push(0);
            for (let shift = 7; shift >= 0; shift -= 1) bytes.push(Math.floor(bitLength / (2 ** (shift * 8))) & 0xff);
            const rotate = (value, bits) => (value >>> bits) | (value << (32 - bits));
            for (let offset = 0; offset < bytes.length; offset += 64) {
                const words = new Uint32Array(64);
                for (let index = 0; index < 16; index += 1) {
                    const position = offset + index * 4;
                    words[index] = ((bytes[position] << 24) | (bytes[position + 1] << 16) | (bytes[position + 2] << 8) | bytes[position + 3]) >>> 0;
                }
                for (let index = 16; index < 64; index += 1) {
                    const value = words[index - 15];
                    const s0 = rotate(value, 7) ^ rotate(value, 18) ^ (value >>> 3);
                    const next = words[index - 2];
                    const s1 = rotate(next, 17) ^ rotate(next, 19) ^ (next >>> 10);
                    words[index] = (words[index - 16] + s0 + words[index - 7] + s1) >>> 0;
                }
                let [a, b, c, d, e, f, g, h] = state;
                for (let index = 0; index < 64; index += 1) {
                    const s1 = rotate(e, 6) ^ rotate(e, 11) ^ rotate(e, 25);
                    const choice = (e & f) ^ (~e & g);
                    const first = (h + s1 + choice + constants[index] + words[index]) >>> 0;
                    const s0 = rotate(a, 2) ^ rotate(a, 13) ^ rotate(a, 22);
                    const majority = (a & b) ^ (a & c) ^ (b & c);
                    const second = (s0 + majority) >>> 0;
                    h = g;
                    g = f;
                    f = e;
                    e = (d + first) >>> 0;
                    d = c;
                    c = b;
                    b = a;
                    a = (first + second) >>> 0;
                }
                state[0] = (state[0] + a) >>> 0;
                state[1] = (state[1] + b) >>> 0;
                state[2] = (state[2] + c) >>> 0;
                state[3] = (state[3] + d) >>> 0;
                state[4] = (state[4] + e) >>> 0;
                state[5] = (state[5] + f) >>> 0;
                state[6] = (state[6] + g) >>> 0;
                state[7] = (state[7] + h) >>> 0;
            }
            return state.map((value) => value.toString(16).padStart(8, '0')).join('');
        }

        function credentialFrameHtml(capabilityCommitment) {
            const serializedCommitment = JSON.stringify(capabilityCommitment);
            return `<!doctype html>
<html><head><meta charset="utf-8"><style>
:root { color-scheme: light dark; font: 14px/1.4 sans-serif; }
body { margin: 0; padding: 20px; background: Canvas; color: CanvasText; }
form { display: grid; gap: 10px; }
label { display: grid; gap: 4px; }
input[type="url"], input[type="text"], input[type="password"] { box-sizing: border-box; border: 1px solid #888; border-radius: 4px; font: inherit; padding: 7px; width: 100%; }
fieldset { border: 1px solid #b33; border-radius: 4px; color: #b33; display: grid; gap: 6px; padding: 8px; }
fieldset[hidden] { display: none; }
.actions { display: flex; gap: 8px; justify-content: flex-end; }
button { border: 1px solid #1677ff; border-radius: 4px; background: #1677ff; color: #fff; cursor: pointer; font: inherit; padding: 7px 12px; }
button.secondary { background: transparent; color: inherit; }
button:disabled { cursor: wait; opacity: .65; }
#status { min-height: 1.4em; }
</style></head><body>
<form id="form" hidden>
<h2>配置 / 重新绑定 TorrentFS</h2>
<label>服务地址<input id="base" type="url" autocomplete="url" required placeholder="https://torrentfs.example.com/base"></label>
<label>用户名<input id="username" type="text" autocomplete="username" required></label>
<label>密码<input id="password" type="password" autocomplete="current-password" required></label>
<fieldset id="risk" hidden><strong>HTTP 明文传输风险</strong><span>当前地址使用 HTTP。用户名、密码、Bearer token、torrent 元数据及上传内容可能被观察、窃取或篡改。继续表示你理解并自行承担该风险。</span><label><span><input id="consent" type="checkbox"> 我理解并承担此 HTTP 风险</span></label></fieldset>
<div id="status" role="status"></div>
<div class="actions"><button id="cancel" class="secondary" type="button">取消</button><button id="submit" type="submit">登录并绑定</button></div>
</form>
<script>
(function () {
    const capabilityCommitment = ${serializedCommitment};
    const sha256Hex = ${sha256Hex.toString()};
    const challengeBytes = new Uint8Array(32);
    crypto.getRandomValues(challengeBytes);
    const challenge = Array.from(challengeBytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
    let nonce;
    let port;
    let initializing = false;
    let initialized = false;
    const form = document.getElementById('form');
    const base = document.getElementById('base');
    const username = document.getElementById('username');
    const password = document.getElementById('password');
    const risk = document.getElementById('risk');
    const consent = document.getElementById('consent');
    const status = document.getElementById('status');
    const submit = document.getElementById('submit');
    const cancel = document.getElementById('cancel');
    const setStatus = (message) => { status.textContent = message; };
    const updateRisk = () => {
        let isHttp = false;
        try { isHttp = new URL(base.value.trim()).protocol === 'http:'; } catch { isHttp = false; }
        risk.hidden = !isHttp;
        consent.required = isHttp;
        if (!isHttp) consent.checked = false;
    };
    base.addEventListener('input', updateRisk);
    window.addEventListener('message', async (event) => {
        if (initialized || initializing || event.source !== window.parent || !event.data || event.data.type !== 'torrentfs-config-init' || event.data.challenge !== challenge || typeof event.data.capability !== 'string' || typeof event.data.nonce !== 'string' || !event.ports[0]) return;
        initializing = true;
        const valid = typeof event.data.capability === 'string' && sha256Hex(event.data.capability) === capabilityCommitment;
        if (!valid || initialized) {
            initializing = false;
            return;
        }
        initialized = true;
        nonce = event.data.nonce;
        port = event.ports[0];
        port.onmessage = (messageEvent) => {
            const data = messageEvent.data;
            if (!data || data.nonce !== nonce || data.type !== 'result') return;
            setStatus(data.message || (data.ok ? '绑定成功。' : '绑定失败。'));
            submit.disabled = false;
            cancel.disabled = false;
        };
        port.start();
        port.postMessage({ nonce, type: 'ready' });
        if (event.data.profile) {
            base.value = event.data.profile.baseUrl || '';
            username.value = event.data.profile.username || '';
        }
        form.hidden = false;
        updateRisk();
        base.focus();
    });
    window.parent.postMessage({ type: 'torrentfs-config-ready', challenge }, '*');
    form.addEventListener('submit', (event) => {
        event.preventDefault();
        updateRisk();
        let parsed;
        try { parsed = new URL(base.value.trim()); } catch { parsed = null; }
        if (!parsed || (parsed.protocol !== 'http:' && parsed.protocol !== 'https:')) {
            setStatus('请输入有效的 HTTP 或 HTTPS 地址。');
            return;
        }
        if (parsed.protocol === 'http:' && !consent.checked) {
            setStatus('请先确认 HTTP 明文传输风险。');
            return;
        }
        if (!username.value.trim() || !password.value) {
            setStatus('请输入用户名和密码。');
            return;
        }
        if (!port) {
            setStatus('配置界面尚未准备好，请重试。');
            return;
        }
        port.postMessage({ nonce, type: 'submit', baseUrl: base.value, username: username.value, password: password.value, httpConsent: consent.checked });
        password.value = '';
        submit.disabled = true;
        cancel.disabled = true;
        setStatus('正在登录 TorrentFS…');
    });
    cancel.addEventListener('click', () => {
        password.value = '';
        port?.postMessage({ nonce, type: 'cancel' });
    });
})();
</script></body></html>`;
        }

        function openCredentialForm(initialConnection, onSubmit, onCancel, onClose) {
            ensureStyles();
            if (typeof MessageChannel !== 'function') {
                setStatus('error', '当前浏览器不支持安全配置界面。');
                return { close() {} };
            }
            let closed = false;
            let frameCleanup = () => {};
            const close = () => {
                if (closed) {
                    return;
                }
                closed = true;
                frameCleanup();
                onClose?.();
            };
            Promise.resolve().then(() => {
                if (closed) {
                    return;
                }
                let capability = randomHex(32);
                const capabilityCommitment = sha256Hex(capability);
                const nonce = randomNonce();
                const channel = new MessageChannel();
                const overlay = document.createElement('div');
                overlay.id = 'torrentfs-mteam-config-overlay';
                const frame = document.createElement('iframe');
                frame.title = 'TorrentFS configuration';
                frame.setAttribute('sandbox', 'allow-scripts');
                frame.srcdoc = credentialFrameHtml(capabilityCommitment);
                const expectedSrcdoc = frame.srcdoc;
                overlay.appendChild(frame);
                let receiverReady = false;
                let initialized = false;
                let readyTimer;
                let submitting = false;
                let receiverListener;
                const closeFrame = () => {
                    pageWindow.clearTimeout(readyTimer);
                    if (receiverListener) pageWindow.removeEventListener('message', receiverListener);
                    channel.port1.onmessage = null;
                    channel.port1.close();
                    capability = '';
                    frame.remove();
                    overlay.remove();
                };
                frameCleanup = closeFrame;
                const sendResult = (result) => {
                    if (closed || !initialized) {
                        return;
                    }
                    channel.port1.postMessage({
                        nonce,
                        type: 'result',
                        ok: result.ok === true,
                        message: result.message || (result.ok ? '绑定成功。' : '绑定失败。')
                    });
                    pageWindow.setTimeout(close, result.ok ? 500 : 1200);
                };
                channel.port1.onmessage = (event) => {
                    const data = event.data;
                    if (closed || !data || data.nonce !== nonce) {
                        return;
                    }
                    if (data.type === 'ready') {
                        initialized = true;
                        return;
                    }
                    if (data.type === 'cancel') {
                        onCancel?.();
                        close();
                        return;
                    }
                    if (data.type !== 'submit' || submitting || !initialized) {
                        return;
                    }
                    submitting = true;
                    let credentials;
                    try {
                        credentials = {
                            baseUrl: typeof data.baseUrl === 'string' ? data.baseUrl : '',
                            username: typeof data.username === 'string' ? data.username : '',
                            password: typeof data.password === 'string' ? data.password : '',
                            httpConsent: data.httpConsent === true
                        };
                        data.password = '';
                        Promise.resolve(onSubmit(credentials)).then((result) => {
                            credentials.password = '';
                            sendResult(result || { ok: false, message: '绑定失败，请重试。' });
                        }).catch(() => {
                            credentials.password = '';
                            sendResult({ ok: false, message: '绑定失败，请重试。' });
                        });
                    } catch {
                        sendResult({ ok: false, message: '绑定失败，请重试。' });
                    }
                };
                channel.port1.start();
                receiverListener = (event) => {
                    const data = event.data;
                    if (closed || receiverReady || event.source !== frame.contentWindow || !data || data.type !== 'torrentfs-config-ready' || typeof data.challenge !== 'string') {
                        return;
                    }
                    if (frame.srcdoc !== expectedSrcdoc || frame.getAttribute('sandbox') !== 'allow-scripts') {
                        close();
                        return;
                    }
                    receiverReady = true;
                    try {
                        frame.contentWindow.postMessage({
                            type: 'torrentfs-config-init',
                            challenge: data.challenge,
                            capability,
                            nonce,
                            profile: {
                                baseUrl: initialConnection?.baseUrl || '',
                                username: initialConnection?.username || ''
                            }
                        }, '*', [channel.port2]);
                        capability = '';
                    } catch {
                        setStatus('error', '配置界面无法启动，请重试。');
                        close();
                    }
                };
                pageWindow.addEventListener('message', receiverListener);
                readyTimer = pageWindow.setTimeout(() => {
                    if (!initialized && !closed) {
                        setStatus('error', '配置界面无法启动，请检查浏览器对 sandbox iframe 的支持。');
                        close();
                    }
                }, 5000);
                (document.body || document.documentElement).appendChild(overlay);
            }).catch(() => {
                if (!closed) {
                    setStatus('error', '配置界面无法启动，请重试。');
                    close();
                }
            });
            return { close };
        }

        function createAction(onClick, fixed) {
            ensureStyles();
            const element = document.createElement('button');
            element.type = 'button';
            element.className = 'torrentfs-mteam-button';
            element.dataset.fixed = fixed ? 'true' : 'false';
            element.textContent = '发送到 TorrentFS';
            const listener = (event) => {
                event.preventDefault();
                onClick();
            };
            element.addEventListener('click', listener);
            return {
                element,
                setBusy(busy) {
                    element.disabled = busy;
                    element.textContent = busy ? '提交中…' : '发送到 TorrentFS';
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
        if (error.kind === 'mteam-token') {
            return 'M-Team 下载地址获取失败，请刷新详情页或重新登录。';
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

        start() {
            if (!this.matches()) {
                return;
            }
            ui.ensureStyles();
            pageBridge.install();

            let candidate;
            let candidateRouteHref = '';
            let routeKey = '';
            let action;
            let activeController;
            let credentialSession;
            let bindingController;
            let lastHref = pageWindow.location.href;
            let mountTimer;

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

            const findMount = () => {
                const appContent = document.querySelector(MTEAM.selectors.appContent);
                if (appContent) {
                    const preferred = appContent.querySelector(MTEAM.selectors.preferredMount);
                    const cell = preferred?.closest('td');
                    if (cell) {
                        return { element: cell, fixed: false };
                    }
                }
                const fallback = document.querySelector(MTEAM.selectors.fallbackMount);
                if (fallback) {
                    return { element: fallback, fixed: false };
                }
                if (document.body) {
                    return { element: document.body, fixed: true };
                }
                return null;
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
                if (!candidate || action) {
                    return;
                }
                const mount = findMount();
                if (!mount) {
                    return;
                }
                action = ui.createAction(() => submit(candidate, action), mount.fixed);
                mount.element.appendChild(action.element);
                const connection = storage.getConnection();
                ui.setStatus(connection?.token ? 'ready' : 'unbound', connectionStatus(connection));
            };

            const handleDetail = (value) => {
                const responseRouteHref = value && typeof value.routeHref === 'string' ? value.routeHref : '';
                const currentRouteHref = pageWindow.location.href;
                if (!responseRouteHref || responseRouteHref !== currentRouteHref || !value.candidate || value.candidate.id === undefined || value.candidate.id === null || !this.matches()) {
                    return;
                }
                const nextCandidate = {
                    id: String(value.candidate.id),
                    name: typeof value.candidate.name === 'string' ? value.candidate.name : '',
                    originFileName: typeof value.candidate.originFileName === 'string' ? value.candidate.originFileName : '',
                    smallDescr: typeof value.candidate.smallDescr === 'string' ? value.candidate.smallDescr : ''
                };
                const nextRouteKey = `${responseRouteHref}:${nextCandidate.id}`;
                lastHref = responseRouteHref;
                if (routeKey !== nextRouteKey) {
                    disposeAction();
                    ui.clearStatus();
                    routeKey = nextRouteKey;
                }
                candidate = nextCandidate;
                candidateRouteHref = responseRouteHref;
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
                    action.dispose();
                    action = undefined;
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
                routeDispose();
                pageWindow.removeEventListener('popstate', routeChanged);
                pageWindow.removeEventListener('hashchange', routeChanged);
                pageWindow.clearTimeout(mountTimer);
                observer.disconnect();
                closeCredentialSession();
                disposeAction();
            };
        }
    };

    function registerPairingCommands(openConfig, unpair) {
        if (typeof GM_registerMenuCommand !== 'function') {
            return;
        }
        GM_registerMenuCommand('配置 / 重新绑定 TorrentFS', () => openConfig());
        GM_registerMenuCommand('解除 TorrentFS 绑定', () => { void unpair(); });
    }

    function bootstrap() {
        if (mteamProvider.matches()) {
            mteamProvider.start();
        }
    }

    bootstrap();
})();
