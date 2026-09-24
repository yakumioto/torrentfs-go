// ==UserScript==
// @name         TorrentFS M-Team bridge
// @namespace    https://github.com/yakumioto/torrentfs-go
// @version      0.1.0
// @description  Send a torrent from an M-Team detail page to a local TorrentFS instance.
// @match        https://m-team.cc/detail/*
// @match        https://*.m-team.cc/detail/*
// @match        https://m-team.io/detail/*
// @match        https://*.m-team.io/detail/*
// @match        http://localhost/*
// @match        http://127.0.0.1/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_deleteValue
// @grant        GM_registerMenuCommand
// @grant        GM_addStyle
// @grant        unsafeWindow
// @connect      localhost
// @connect      127.0.0.1
// @connect      m-team.cc
// @connect      m-team.io
// @run-at       document-start
// @license      MPL-2.0
// ==/UserScript==

(function () {
    'use strict';

    const STORAGE_KEY = 'torrentfs.connection';
    const SESSION_TOKEN_KEY = 'torrentfs.access-token';
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
        if (!value || typeof value !== 'object' || typeof value.baseUrl !== 'string' || typeof value.token !== 'string') {
            return null;
        }
        if (!value.baseUrl || !value.token) {
            return null;
        }
        return {
            baseUrl: value.baseUrl,
            token: value.token,
            pairedAt: Number.isFinite(value.pairedAt) ? value.pairedAt : 0
        };
    }

    function saveConnection(baseUrl, token) {
        GM_setValue(STORAGE_KEY, {
            baseUrl,
            token,
            pairedAt: Date.now()
        });
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
            clearConnection();
        }
    }

    function readWebUIToken() {
        try {
            const token = pageWindow.sessionStorage.getItem(SESSION_TOKEN_KEY);
            return token || '';
        } catch {
            return '';
        }
    }

    function currentLoopbackOrigin() {
        const location = pageWindow.location;
        if (location.protocol !== 'http:' || !['localhost', '127.0.0.1'].includes(location.hostname)) {
            throw makeError('pairing', '只能从 localhost 或 127.0.0.1 的 TorrentFS Web UI 绑定。');
        }
        return location.origin;
    }

    function normalizeBaseUrl(value) {
        let url;
        try {
            url = new URL(value);
        } catch {
            throw makeError('pairing', 'TorrentFS 地址无效。');
        }
        if (url.protocol !== 'http:' || !['localhost', '127.0.0.1'].includes(url.hostname)) {
            throw makeError('pairing', '只支持绑定本机 TorrentFS 服务。');
        }
        return url.origin;
    }

    // storage
    const storage = {
        getConnection,
        saveConnection,
        clearConnection,
        clearConnectionIfTokenMatches,
        readWebUIToken,
        currentLoopbackOrigin,
        normalizeBaseUrl,
        isLoopbackPage() {
            try {
                currentLoopbackOrigin();
                return true;
            } catch {
                return false;
            }
        }
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
                    return;
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
                    timeout: options.timeout,
                    responseType: options.responseType,
                    anonymous: options.anonymous,
                    onload: (response) => finish(resolve, response),
                    onerror: () => finish(reject, makeError('transport', '网络请求失败。')),
                    ontimeout: () => finish(reject, makeError('timeout', '请求超时。')),
                    onabort: () => finish(reject, makeError('aborted', '请求已取消。'))
                });
            } catch (error) {
                finish(reject, error instanceof Error ? error : makeError('transport', '网络请求失败。'));
            }
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

    // torrentfsClient
    const torrentfsClient = {
        async verifyPairing(baseUrl, token, signal) {
            const normalizedBaseUrl = normalizeBaseUrl(baseUrl);
            let response;
            try {
                response = await gmRequest({
                    method: 'GET',
                    url: `${normalizedBaseUrl}/api/v1/torrents`,
                    headers: {
                        Accept: 'application/json',
                        Authorization: `Bearer ${token}`
                    },
                    timeout: REQUEST_TIMEOUT,
                    signal,
                    anonymous: true
                });
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                throw makeError('pairing', '无法连接 TorrentFS Web UI。');
            }
            if (response.status === 401) {
                throw makeError('unauthorized', 'TorrentFS 会话已失效。', 401);
            }
            if (response.status < 200 || response.status >= 300) {
                throw makeError('pairing', `TorrentFS 配对检查失败（HTTP ${response.status}）。`, response.status);
            }
            return normalizedBaseUrl;
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
                response = await gmRequest({
                    method: 'GET',
                    url: parsed.href,
                    timeout: REQUEST_TIMEOUT,
                    responseType: 'arraybuffer',
                    signal,
                    anonymous: false
                });
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                if (error.kind === 'timeout') {
                    throw makeError('mteam-download', 'M-Team 下载超时，请重试。');
                }
                throw makeError('mteam-download', 'M-Team 种子下载失败，请刷新详情页或重新登录。');
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
            const token = connection.token;
            const form = new FormData();
            form.append('file', new Blob([bytes], { type: 'application/x-bittorrent' }), filename);
            let response;
            try {
                response = await gmRequest({
                    method: 'POST',
                    url: `${normalizeBaseUrl(connection.baseUrl)}/api/v1/torrents`,
                    headers: {
                        Accept: 'application/json',
                        Authorization: `Bearer ${token}`
                    },
                    data: form,
                    timeout: REQUEST_TIMEOUT,
                    signal,
                    anonymous: true
                });
            } catch (error) {
                if (isAbortError(error)) {
                    throw error;
                }
                throw makeError('upload-unknown', '上传结果未知，可重试。');
            }
            if (response.status === 401) {
                storage.clearConnectionIfTokenMatches(token);
                throw makeError('unauthorized', 'TorrentFS 会话已失效，请回到 Web UI 重新绑定。', 401);
            }
            if (response.status !== 201) {
                throw makeError('upload', statusMessage(response.status), response.status);
            }
            return responseJson(response);
        },

        async pairCurrentPage() {
            const baseUrl = storage.currentLoopbackOrigin();
            const token = storage.readWebUIToken();
            if (!token) {
                throw makeError('pairing', '请先在 TorrentFS Web UI 登录，并启用 HTTP auth。');
            }
            await this.verifyPairing(baseUrl, token);
            storage.saveConnection(baseUrl, token);
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

        return { ensureStyles, setStatus, clearStatus, createAction };
    })();

    function userMessage(error) {
        if (!error) {
            return '提交失败，请重试。';
        }
        if (error.kind === 'pairing') {
            return error.message;
        }
        if (error.kind === 'unauthorized') {
            return 'TorrentFS 会话已失效，请回到 Web UI 重新绑定。';
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

            const submit = async (selectedCandidate, selectedAction) => {
                if (activeController) {
                    return;
                }
                const controller = new AbortController();
                activeController = controller;
                selectedAction.setBusy(true);
                ui.setStatus('busy', '正在获取 M-Team 种子并提交到 TorrentFS…');
                try {
                    const connection = storage.getConnection();
                    if (!connection) {
                        throw makeError('pairing', '尚未绑定 TorrentFS，请在 Web UI 菜单中绑定当前服务。');
                    }
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
                if (storage.getConnection()) {
                    ui.setStatus('ready', 'TorrentFS 已绑定，可提交当前详情。');
                } else {
                    ui.setStatus('unbound', 'TorrentFS 未绑定，请先在本机 Web UI 菜单中绑定。');
                }
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
                disposeAction();
            };
        }
    };

    function registerPairingCommands() {
        if (typeof GM_registerMenuCommand !== 'function') {
            return;
        }
        GM_registerMenuCommand('绑定当前 TorrentFS', async () => {
            try {
                await torrentfsClient.pairCurrentPage();
                pageWindow.alert('TorrentFS 绑定成功。');
            } catch (error) {
                if (error.kind === 'unauthorized') {
                    storage.clearConnection();
                }
                pageWindow.alert(userMessage(error));
            }
        });
        GM_registerMenuCommand('解除 TorrentFS 绑定', () => {
            storage.clearConnection();
            pageWindow.alert('TorrentFS 绑定已解除。');
        });
    }

    function bootstrap() {
        if (storage.isLoopbackPage()) {
            registerPairingCommands();
            return;
        }
        if (mteamProvider.matches()) {
            mteamProvider.start();
        }
    }

    bootstrap();
})();
