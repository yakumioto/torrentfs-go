import {
  Alert,
  Badge,
  Button,
  Code,
  Group,
  Loader,
  Modal,
  NativeSelect,
  NumberInput,
  Stack,
  Switch,
  TextInput,
} from '@mantine/core';
import {
  IconAlertTriangle,
  IconDeviceFloppy,
  IconInfoCircle,
  IconRefresh,
  IconSearch,
} from '@tabler/icons-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { useLocation } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import { useConfig, useSaveConfig } from '../queries/hooks';
import type { ConfigKey, ConfigSaveResult, ConfigView } from '../types/api';
import { configValueInputError } from '../utils/config-value';
import { useUnsavedChangesGuard } from '../utils/unsaved-changes';
import { usePageVisible } from '../utils/visibility';
import {
  dangerousCategoryLabel,
  dangerousConfigChanges,
  dangerousConfigKey,
  type DangerousConfigKey,
} from '../utils/config-safety';
import {
  configSaveDurabilityUnconfirmed,
  configSaveFailure,
  userFacingConfigSaveError,
} from '../utils/user-facing-error';
import styles from './ConfigPage.module.css';

/**
 * Chinese names of the configuration sections. The configuration schema names
 * the sections by their TOML table, so the page translates the known ones and
 * falls back to the path for a section a later schema adds.
 */
const SECTION_LABELS: Record<string, string> = {
  '': '基础',
  connections: '连接',
  proxy: '代理',
  cache: '缓存',
  identity: '身份',
  http: 'HTTP 服务',
  'http.auth': 'HTTP 认证',
  strm: 'STRM 流媒体',
  log: '日志',
  filesystem: '文件系统',
  fuse: 'FUSE',
  smb: 'SMB',
  upload_rate: '上传限速',
  experiments: '实验',
  'experiments.upload': '上传实验',
};

/**
 * Domain copy for a category whose heading alone does not explain what the
 * grouped configuration controls.
 */
const CATEGORY_DESCRIPTIONS: Record<string, string> = {
  experiments:
    '上传统计实验常驻运行，默认基线模式；在这里统一调整实验模式与参数，改动按标记立即生效或等待重启。',
};

const SOURCE_LABELS: Record<string, string> = {
  default: '内置默认',
  file: '配置文件',
  environment: '环境变量',
};

/** Chinese names of the schema value types. */
const KIND_LABELS: Record<string, string> = {
  string: '字符串',
  bool: '布尔',
  int: '整数',
  int64: '整数',
  duration: '时长',
  byte_quantity: '字节量',
  byte_rate: '速率',
  percent: '百分比',
  ratio: '倍率',
  list: '列表',
  enum: '枚举',
};

/**
 * Schema kinds whose editor control already conveys the expected format
 * (a dropdown, a switch, a number field), so the kind badge next to the path
 * would only repeat what the control already shows. Every other kind edits
 * through a plain text field and keeps the badge to say what format it wants.
 */
const STRUCTURED_KINDS = new Set(['enum', 'bool', 'int']);

/** Chinese names of the process components that own a configuration key. */
const COMPONENT_LABELS: Record<string, string> = {
  process: '进程',
  api: 'HTTP 服务',
  auth: '认证',
  cache: '缓存',
  experiment: '实验',
  filesystem: '文件系统',
  fuse: 'FUSE',
  logging: '日志',
  session: '会话',
  smb: 'SMB',
  stream: '流媒体',
};

/**
 * Chinese translations of each configuration key's documentation, by its dot
 * path. The schema documentation is generated from the English doc comments
 * of the configuration struct (see internal/config/schema_documentation.go),
 * so translating it here keeps the change inside the web package instead of
 * editing the Go source comments, following the same path/label pattern
 * already used for SECTION_LABELS, SOURCE_LABELS, KIND_LABELS and
 * COMPONENT_LABELS above. A path without a translation falls back to the
 * documentation the API returned, so a later schema addition still shows
 * something rather than nothing.
 */
const DOCUMENTATION_LABELS: Record<string, string> = {
  backend: '后端（backend）选择输出方式：none（不输出）、fuse 或 smb。',
  'cache.capacity': '缓存容量是内存中最多保留的字节数。',
  'connections.bootstrap_nodes':
    '引导节点（bootstrap nodes）用显式的 host:port 列表覆盖默认的 DHT 引导主机。每个条目按地址族解析，并按发起请求的 socket 所属地址族过滤。留空则使用客户端内置的引导列表。',
  'connections.disable_ipv4': '关闭 IPv4 后，所有 IPv4 的监听、拨号与 DHT 服务都会停用。',
  'connections.disable_ipv6':
    '关闭 IPv6 后，所有 IPv6 的监听、拨号与 DHT 服务都会停用。两个地址族只能二选一禁用：同时禁用会导致没有可用的传输方式。',
  'connections.listen_host': '监听地址是会话用于接受 peer 连接的地址。留空则由客户端选择默认值。',
  'connections.listen_port': '监听端口是接受 peer 连接的 TCP/UDP 端口。设为 0 会随机选择一个空闲端口。',
  'connections.no_port_forwarding':
    '禁用端口转发会关闭 UPnP/NAT-PMP 端口映射。默认值为 true，这样没有 UPnP 设备的部署，和有设备但未配置映射规则的部署行为一致。',
  'connections.read_grace':
    '读取宽限期是种子在最后一个文件句柄关闭后，仍保留读取连接配额的时长，用于覆盖跳转、暂停或播放器重连等场景，避免因此断开 peer 连接。该值必须为正数。',
  'connections.reading_conns':
    '读取连接数是正在被读取的种子（文件句柄处于打开状态，或在读取宽限期内曾经打开过）可使用的 peer 连接配额。如果种子的全部内容已在分片缓存中，则不会用到该配额，因为播放直接由缓存提供。该值至少为 1。',
  'connections.uploading_conns':
    '上传连接数是种子在没有被读取、仅处于上传状态时可使用的 peer 连接配额。没有全局配额上限，每个种子的配额独立计算。设为 0 会关闭上传配额，已缓存的种子只保留一条探测连接。',
  'experiments.upload.announce_log':
    'Announce 日志是每次向 tracker 上报时写入一条记录的 JSON Lines 文件。留空则禁用。文件在启动时打开，修改后的值需要重启才能生效。',
  'experiments.upload.cap': '上报上限是单个种子累计上报上传量的上限。',
  'experiments.upload.jitter': '抖动百分比是每次上报相对标准增量的偏差幅度，默认 20%；设为 0 则不加入偏差。',
  'experiments.upload.mode':
    '实验模式选择上报方式：baseline（基线）、inject（注入）或 amplify（放大）。默认是 baseline，即如实上报真实计数的对照组。不会自动推断模式：配置了速率或倍率时，必须同时指定对应的模式，否则实验无法启动。',
  'experiments.upload.multiplier': '放大倍率是在 amplify 模式下，按真实传输字节数的多少倍放大上报的上传量。',
  'experiments.upload.peer_sample_interval':
    'Peer 采样间隔是多久把一次 peer 活动采样写入 announce 日志。该值在启动时读取，修改后需要重启才能生效。',
  'experiments.upload.rate': '虚报速率是在 inject 模式下，上报上传量的平均虚增速度。',
  'experiments.upload.seed':
    '随机种子用于固定抖动序列，便于复现实验结果。设为 0 则由进程自行选择。种子在启动时确定，修改后需要重启才能生效。',
  'filesystem.read_timeout': '读取超时限制单次从种子读取一个分片所允许的最长时间。',
  'fuse.allow_other':
    '允许其他用户会向内核传递 allow_other 选项，解除 FUSE 默认只允许挂载用户访问挂载点的限制。',
  'fuse.mountpoint': '挂载点是文件树挂载到的目录。',
  'fuse.strm_mountpoint': 'STRM 挂载点是签名流媒体视图挂载到的目录。',
  'http.auth.enabled': '启用后会开启 HTTP 登录认证。当监听地址不是回环地址时，必须启用该项。',
  'http.auth.token_ttl':
    '登录令牌有效期是令牌允许的最长空闲时间，每次有效请求都会顺延该窗口；令牌只保存在内存中，随进程退出而失效。',
  'http.listen_addr': '监听地址是 HTTP 服务绑定的地址。留空则禁用该服务，默认只绑定回环地址。',
  'http.max_upload_size': '最大上传大小限制任何上传请求体的体积，包括 .torrent 文件上传和字幕文件托管上传。',
  'identity.extended_handshake_client_version':
    '扩展握手客户端版本会作为 BEP 10 扩展握手的 v 字段发送。',
  'identity.peer_id_prefix': 'Peer ID 前缀是生成的 20 字节 BitTorrent peer ID 的前缀部分。',
  'identity.tracker_user_agent': 'Tracker User-Agent 会作为 HTTP tracker announce 请求的 User-Agent 请求头发送。',
  'log.add_source': '添加来源信息后，每条日志都会附带源码文件名和行号。',
  'log.format': '日志格式决定日志记录的样式：text 面向人工阅读，json 面向日志采集系统。',
  'log.level': '日志级别是记录日志的最低等级：debug、info、warn 或 error。debug 默认关闭。',
  password: '密码是 SMB 输出与 HTTP 登录共用的明文密码。启用 SMB 输出或开启 HTTP 认证时都必须设置，且没有默认值。',
  'proxy.socks5_url': 'SOCKS5 地址是可选的 SOCKS5 或 SOCKS5H 代理端点。',
  'smb.listen_addr': '监听地址是 SMB 服务绑定的地址。',
  'smb.strm': 'STRM 开关控制 SMB 输出里 STRM 共享的状态：auto（自动）、on（开启）或 off（关闭）。',
  'strm.base_url':
    '基础 URL 是播放器可访问的 HTTP(S) 地址，可以带反向代理的路径前缀。留空则禁用签名流媒体链接。',
  'upload_rate.limit': '限速值是所有种子和 peer 合计的上传速率上限。留空则不限速。设置限速时间窗口时，该值必须为正数。',
  'upload_rate.schedule_end':
    '结束时间是每日限速时间窗口的结束点，使用严格的 24 小时制 HH:MM（本地时间），该时刻本身不计入窗口内。起止时间都留空表示全天都限速。',
  'upload_rate.schedule_start':
    '开始时间是每日限速时间窗口的起点，使用严格的 24 小时制 HH:MM（本地时间）。起止时间都留空表示全天都限速。设置该值时必须同时设置结束时间，且开始时间要早于结束时间。',
  username: '用户名是 SMB 输出接受的登录名。HTTP 登录不校验用户名，因此该项只在使用 SMB 输出时才需要设置。',
};

function sectionLabel(section: string): string {
  return SECTION_LABELS[section] ?? section;
}

function sourceLabel(source: string): string {
  return SOURCE_LABELS[source] ?? source;
}

function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind;
}

function componentLabel(component: string): string {
  return COMPONENT_LABELS[component] ?? component;
}

/** The documentation a reader sees: the Chinese translation, or the API's text when a path has none yet. */
function documentationLabel(key: ConfigKey): string {
  return DOCUMENTATION_LABELS[key.path] ?? key.documentation;
}

/** The value a reader sees; a secret is never rendered and an empty value has a label. */
function displayValue(key: ConfigKey, value: string): string {
  if (key.secret) {
    return '••••••';
  }
  return value === '' ? '（空）' : value;
}

/** Whether one key matches the search term. Search covers the metadata and both values. */
function matchesSearch(key: ConfigKey, term: string): boolean {
  const haystack = [
    key.path,
    key.section,
    sectionLabel(key.section),
    key.kind,
    key.documentation,
    documentationLabel(key),
    key.default,
    key.file_value,
    key.effective_value,
    key.environment,
    key.component,
    sourceLabel(key.source),
  ].join('\n').toLowerCase();
  return haystack.includes(term);
}

interface ConfigSection {
  id: string;
  label: string;
  keys: ConfigKey[];
}

interface ConfigCategory {
  id: string;
  label: string;
  description: string;
  sections: ConfigSection[];
}

/**
 * Groups the keys by configuration section, keeping the schema order of the
 * first key of every section.
 */
function groupConfigKeys(keys: ConfigKey[]): ConfigSection[] {
  const sections: ConfigSection[] = [];
  const byID = new Map<string, ConfigSection>();
  for (const key of keys) {
    // 'basics' is a deliberate stand-in for the unsectioned keys, chosen to
    // avoid colliding with the app's own `<div id="root">` mount point: the
    // literal id 'root' used to pick up that div's global `min-height: 100vh`
    // rule (web/src/styles/global.css) and stretch this section to the full
    // viewport height.
    const id = key.section === '' ? 'basics' : key.section;
    let section = byID.get(id);
    if (section === undefined) {
      section = { id, label: sectionLabel(key.section), keys: [] };
      byID.set(id, section);
      sections.push(section);
    }
    section.keys.push(key);
  }
  return sections;
}

/**
 * The category of a section: the first path segment of its TOML table. The
 * `experiments` category therefore collects `[experiments.upload]` and every
 * later experiment subsection, while a plain section is its own category.
 */
function categoryID(sectionID: string): string {
  if (sectionID === 'basics') {
    return 'basics';
  }
  return sectionID.split('.')[0] ?? sectionID;
}

function categoryLabel(id: string): string {
  return id === 'basics' ? '基础' : sectionLabel(id);
}

/** Groups the sections into the first-level categories of the page. */
function groupConfigCategories(keys: ConfigKey[]): ConfigCategory[] {
  const categories: ConfigCategory[] = [];
  const byID = new Map<string, ConfigCategory>();
  for (const section of groupConfigKeys(keys)) {
    const id = categoryID(section.id);
    let category = byID.get(id);
    if (category === undefined) {
      category = {
        id,
        label: categoryLabel(id),
        description: CATEGORY_DESCRIPTIONS[id] ?? '',
        sections: [],
      };
      byID.set(id, category);
      categories.push(category);
    }
    category.sections.push(section);
  }
  return categories;
}

function categoryKeyCount(category: ConfigCategory): number {
  return category.sections.reduce((count, section) => count + section.keys.length, 0);
}

function ReadOnlyBanner({ view }: { view: ConfigView }) {
  if (view.read_only_reason === 'not_configured') {
    return (
      <Alert color="yellow" icon={<IconInfoCircle size={16} />} title="配置页面只读：没有加载配置文件" className={styles.alert}>
        <p className={styles.alertText}>
          后台服务启动时没有加载配置文件。请使用 <Code>-config</Code> 指定配置文件后重启服务，配置页面才能保存改动。
        </p>
      </Alert>
    );
  }
  if (view.read_only_reason === 'file_not_writable') {
    return (
      <Alert color="yellow" icon={<IconInfoCircle size={16} />} title="配置页面只读：配置文件不可写" className={styles.alert}>
        <p className={styles.alertText}>
          配置文件或所在目录没有写权限（例如只读挂载），页面为只读。请调整配置文件和所在目录的写权限后重新加载页面。
        </p>
        {view.config_file !== '' && (
          <p className={styles.alertText}>
            配置文件：<Code>{view.config_file}</Code>
          </p>
        )}
      </Alert>
    );
  }
  return null;
}

/** Render the time of the most recent save or automatic reload for a human. */
function operationTime(at: string | null): string {
  if (at === null) {
    return '';
  }
  const time = new Date(at);
  return Number.isNaN(time.getTime()) ? at : time.toLocaleString('zh-CN', { hour12: false });
}

/**
 * Shows the outcome of the most recent save stored in the configuration view,
 * so a page reload keeps reporting what happened instead of losing it with
 * the mutation state. A save made in this session is shown by its own result
 * block instead; this status is the persisted record.
 */
function SaveStatus({ view }: { view: ConfigView }) {
  const save = view.last_save;
  if (save.at === null) {
    return null;
  }
  if (!save.ok) {
    return (
      <Alert
        color="red"
        icon={<IconAlertTriangle size={16} />}
        title="最近一次保存失败"
        className={styles.alert}
        data-testid="config-save-status"
      >
        <p className={styles.alertText} data-testid="config-save-status-error">{save.error}</p>
        <p className={styles.alertText}>
          时间：<span data-testid="config-save-status-time">{operationTime(save.at)}</span>
        </p>
      </Alert>
    );
  }
  return (
    <Alert
      color="green"
      icon={<IconDeviceFloppy size={16} />}
      title="最近一次保存成功"
      className={styles.alert}
      data-testid="config-save-status"
    >
      <p className={styles.alertText}>
        时间：<span data-testid="config-save-status-time">{operationTime(save.at)}</span>
      </p>
      {save.keys.length > 0 && (
        <ul className={styles.resultKeys} data-testid="config-save-status-keys">
          {save.keys.map((path) => <li key={path}><code className={styles.code}>{path}</code></li>)}
        </ul>
      )}
    </Alert>
  );
}

/** The badge that tells whether a key the reload changed took effect now. */
function reloadKeyLabel(configKey: ConfigKey | undefined): string {
  if (configKey === undefined) {
    return '已更新';
  }
  if (configKey.read_only_reason === 'environment_override') {
    return '环境变量覆盖';
  }
  return configKey.hot_apply ? '立即生效' : '重启生效';
}

/**
 * Shows how the running process last reacted to an external edit of the
 * configuration file: a successful automatic reload, a rejected edit that
 * left the previous configuration in force, or the file error that currently
 * prevents a load. The reread action applies the disk values to the form; the
 * caller asks first when that would discard unsaved drafts.
 */
function ReloadStatus({ view, onReread, rereadDisabled }: {
  view: ConfigView;
  onReread: () => void;
  rereadDisabled: boolean;
}) {
  const reload = view.last_reload;
  const reread = (
    <Button
      variant="light"
      color="torrent"
      size="compact-sm"
      leftSection={<IconRefresh size={14} />}
      onClick={onReread}
      disabled={rereadDisabled}
      data-testid="config-reload-reread"
    >
      重新读取
    </Button>
  );

  if (view.file_error !== '') {
    return (
      <Alert
        color="red"
        icon={<IconAlertTriangle size={16} />}
        title="配置文件当前无法使用"
        className={styles.alert}
        data-testid="config-file-error"
      >
        <p className={styles.alertText}>
          磁盘上的配置文件无法读取或未通过校验，页面与服务继续使用最近一次成功加载的配置。
        </p>
        <p className={styles.alertText}>{view.file_error}</p>
        {reload.at !== null && reload.error !== '' && (
          <p className={styles.alertText} data-testid="config-reload-error">
            最近一次自动重载失败：{reload.error}
          </p>
        )}
        <Group mt="sm">{reread}</Group>
      </Alert>
    );
  }

  if (reload.at === null) {
    return (
      <div className={`panel panel--padding ${styles.reload}`} data-testid="config-reload-status">
        <Group justify="space-between" gap="sm">
          <span className={styles.reloadText}>配置文件监视中：尚未检测到外部修改。</span>
          {reread}
        </Group>
      </div>
    );
  }

  if (!reload.ok) {
    return (
      <Alert
        color="red"
        icon={<IconAlertTriangle size={16} />}
        title="最近一次自动重载失败"
        className={styles.alert}
        data-testid="config-reload-status"
      >
        <p className={styles.alertText}>
          配置文件的外部修改未通过校验，服务继续使用最近一次成功加载的配置。
        </p>
        <p className={styles.alertText} data-testid="config-reload-error">{reload.error}</p>
        <Group mt="sm">{reread}</Group>
      </Alert>
    );
  }

  return (
    <Alert
      color="green"
      icon={<IconRefresh size={16} />}
      title="最近一次自动重载成功"
      className={styles.alert}
      data-testid="config-reload-status"
    >
      <p className={styles.alertText}>
        检测到配置文件的外部修改并已重新加载（{operationTime(reload.at)}）。
      </p>
      {reload.keys.length > 0 && (
        <ul className={styles.resultKeys} data-testid="config-reload-keys">
          {reload.keys.map((path) => {
            const changed = view.keys.find((key) => key.path === path);
            return (
              <li key={path}>
                <code className={styles.code}>{path}</code>{' '}
                <Badge variant="light" color={changed?.hot_apply ? 'teal' : 'gray'}>
                  {reloadKeyLabel(changed)}
                </Badge>
              </li>
            );
          })}
        </ul>
      )}
      <Group mt="sm">{reread}</Group>
    </Alert>
  );
}

/** One editor control derived from the schema type of a configuration key. */
function KeyControl({
  configKey,
  draft,
  editable,
  onChange,
}: {
  configKey: ConfigKey;
  draft: string;
  editable: boolean;
  onChange: (value: string) => void;
}) {
  const label = configKey.path;
  switch (configKey.kind) {
    case 'enum': {
      const options = configKey.values.map((value) => ({
        value,
        label: value === '' ? '（未设置）' : value,
      }));
      if (!configKey.values.includes(draft)) {
        options.push({ value: draft, label: draft === '' ? '（未设置）' : draft });
      }
      return (
        <NativeSelect
          aria-label={label}
          data={options}
          value={draft}
          onChange={(event) => onChange(event.currentTarget.value)}
          disabled={!editable}
          size="xs"
        />
      );
    }
    case 'bool':
      return (
        <Switch
          aria-label={label}
          checked={draft === 'true'}
          onChange={(event) => onChange(event.currentTarget.checked ? 'true' : 'false')}
          disabled={!editable}
          size="sm"
        />
      );
    case 'int': {
      const numeric = draft.trim() === '' ? '' : Number(draft);
      return (
        <NumberInput
          aria-label={label}
          value={Number.isFinite(numeric as number) ? numeric : ''}
          onChange={(value) => onChange(value === '' ? '' : String(value))}
          disabled={!editable}
          allowDecimal={false}
          size="xs"
        />
      );
    }
    default:
      return (
        <TextInput
          aria-label={label}
          value={draft}
          onChange={(event) => onChange(event.currentTarget.value)}
          disabled={!editable}
          placeholder={configKey.kind === 'list' ? '逗号分隔，例如 tracker.example:6881' : undefined}
          size="xs"
        />
      );
  }
}

function KeyCard({
  configKey,
  draft,
  editable,
  onChange,
  error,
}: {
  configKey: ConfigKey;
  draft: string;
  editable: boolean;
  onChange: (value: string) => void;
  error?: string;
}) {
  // The input already shows the value a reader is editing (the file layer,
  // per the config write-back model), so "customized" compares that live
  // value against the default instead of re-stating a source label. A file
  // value that merely repeats the default does not count: the reader cares
  // whether the outcome differs, not whether the file happens to spell it
  // out.
  const customized = draft !== configKey.default;

  return (
    <article className={`panel panel--padding ${styles.key}`} aria-label={configKey.path}>
      <div className={styles.keyHeader}>
        <code className={styles.path}>{configKey.path}</code>
        <Group gap={6} wrap="wrap">
          {!STRUCTURED_KINDS.has(configKey.kind) && (
            <Badge variant="outline" color="gray">{kindLabel(configKey.kind)}</Badge>
          )}
          <Badge variant="light" color={configKey.hot_apply ? 'teal' : 'gray'}>
            {configKey.hot_apply ? '立即生效' : '重启生效'}
          </Badge>
          {configKey.read_only_reason === 'environment_override' && (
            <Badge variant="light" color="yellow">环境变量覆盖</Badge>
          )}
          {configKey.read_only_reason === 'secret' && (
            <Badge variant="light" color="gray">口令只读</Badge>
          )}
        </Group>
      </div>

      {configKey.documentation !== '' && (
        <p className={styles.documentation}>{documentationLabel(configKey)}</p>
      )}

      {configKey.component !== '' && (
        <p className={styles.meta}>所属组件：{componentLabel(configKey.component)}</p>
      )}

      {!configKey.secret && (
        <div className={styles.editor}>
          <span className={styles.editorLabel}>当前值</span>
          <KeyControl configKey={configKey} draft={draft} editable={editable} onChange={onChange} />
          {customized && (
            <button
              type="button"
              className={styles.resetLink}
              onClick={() => onChange(configKey.default)}
              disabled={!editable}
            >
              已自定义 · 恢复默认（{displayValue(configKey, configKey.default)}）
            </button>
          )}
        </div>
      )}

      {configKey.read_only_reason === 'environment_override' && (
        <p className={styles.readOnlyNote}>
          该项由环境变量 <code className={styles.code}>{configKey.environment}</code> 覆盖，当前生效值为{' '}
          <code className={styles.code}>{displayValue(configKey, configKey.effective_value)}</code>
          ，配置文件中的值不会生效。
        </p>
      )}
      {configKey.read_only_reason === 'secret' && (
        <p className={styles.readOnlyNote}>
          口令属于凭据，不会在页面上显示，也不能在这里编辑；保存其他配置时它会原样保留。
        </p>
      )}
      {error !== undefined && (
        <p className={styles.fieldError} role="alert">{error}</p>
      )}
    </article>
  );
}

function SaveResultGroups({ result }: { result: ConfigSaveResult }) {
  return (
    <div className={styles.result}>
      <p className={styles.resultSummary} data-testid="config-save-summary">
        已生效 {result.applied.length} 项，重启后生效 {result.restart_required.length} 项。
      </p>
      <div className={styles.resultGroup} data-testid="config-applied-group">
        <strong>已生效（{result.applied.length} 项）</strong>
        <p className={styles.resultNote}>已应用到运行中的进程。</p>
        <ul className={styles.resultKeys} data-testid="config-applied-keys">
          {result.applied.length === 0
            ? <li className={styles.resultEmpty}>无</li>
            : result.applied.map((path) => <li key={path}><code className={styles.code}>{path}</code></li>)}
        </ul>
      </div>
      <div className={styles.resultGroup} data-testid="config-restart-group">
        <strong>重启后生效（{result.restart_required.length} 项）</strong>
        <p className={styles.resultNote}>已写入配置文件，重启服务后才采用新值。</p>
        <ul className={styles.resultKeys} data-testid="config-restart-keys">
          {result.restart_required.length === 0
            ? <li className={styles.resultEmpty}>无</li>
            : result.restart_required.map((path) => <li key={path}><code className={styles.code}>{path}</code></li>)}
        </ul>
      </div>
      {!result.persisted && (
        <p className={styles.resultWarn}>文件持久化未确认：重启后可能无法保留这些改动。</p>
      )}
    </div>
  );
}

function DangerousSaveDialog({
  opened,
  changes,
  onCancel,
  onConfirm,
}: {
  opened: boolean;
  changes: DangerousConfigKey[];
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <Modal
      opened={opened}
      onClose={onCancel}
      title="危险配置项确认"
      centered
      closeButtonProps={{ 'aria-label': '关闭弹窗' }}
    >
      <Stack gap="sm">
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} className={styles.modalAlert}>
          以下改动可能会让服务在重启后失联，或改变输出形态与挂载位置。请确认这是预期行为。
        </Alert>
        <ul className={styles.dangerList} data-testid="config-danger-list">
          {changes.map((change) => (
            <li key={change.path} className={styles.dangerItem}>
              <div className={styles.dangerHeader}>
                <code className={styles.code}>{change.path}</code>
                <Badge variant="light" color="red">{dangerousCategoryLabel(change.category)}</Badge>
              </div>
              <p className={styles.dangerImpact}>{change.impact}</p>
            </li>
          ))}
        </ul>
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onCancel}>取消</Button>
          <Button color="red" onClick={onConfirm}>仍然保存</Button>
        </Group>
      </Stack>
    </Modal>
  );
}

interface SaveOutcome {
  code: string;
  result: ConfigSaveResult;
}

export function ConfigPage() {
  const auth = useAuth();
  const location = useLocation();
  const visible = usePageVisible();
  const query = useConfig(auth.api, auth.isReady && visible);
  const save = useSaveConfig(auth.api);

  const [search, setSearch] = useState('');
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  // The file values the drafts are compared against. A successful save moves
  // the edited keys here, so the form is clean without waiting for a refetch.
  const [saved, setSaved] = useState<Record<string, string>>({});
  const [seeded, setSeeded] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [saveOutcome, setSaveOutcome] = useState<SaveOutcome | null>(null);
  const [saveMessage, setSaveMessage] = useState('');
  const [dangerOpen, setDangerOpen] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [rereadConfirmOpen, setRereadConfirmOpen] = useState(false);
  // The baseline of the view the drafts were seeded from. Saving against this
  // baseline instead of whatever the view holds now turns an external edit
  // that landed after the seed into a conflict, instead of silently rebasing
  // stale drafts onto a file the page never showed.
  const [seededBaseline, setSeededBaseline] = useState('');
  // The baseline sent with the save whose refreshed view has not been adopted
  // yet; it lets the post-save refetch move the seeded baseline forward.
  const pendingSaveBaseline = useRef<string | null>(null);
  const durabilityRefetched = useRef<unknown>(null);
  // The hash the page already scrolled to; a background refetch must not pull
  // the reader away from wherever they scrolled to after following the link.
  const scrolledHash = useRef('');
  // The category currently highlighted in the sidebar: the one nearest the
  // top of the viewport while scrolling, or the one a nav click targeted.
  const [activeCategoryID, setActiveCategoryID] = useState('');

  const view = query.data;
  const refetch = query.refetch;

  const seedFromView = useCallback((next: ConfigView) => {
    const values: Record<string, string> = {};
    for (const key of next.keys) {
      values[key.path] = key.file_value;
    }
    setDrafts(values);
    setSaved(values);
    setSeededBaseline(next.baseline);
    setSeeded(true);
    setFieldErrors({});
    setSaveOutcome(null);
    setSaveMessage('');
    setDangerOpen(false);
    setRereadConfirmOpen(false);
    pendingSaveBaseline.current = null;
  }, []);

  // Seed the form from the file layer once. A background refetch must not
  // replace what the user is editing; only an explicit reload (and a fresh
  // save response) may move the comparison baseline.
  useEffect(() => {
    if (view === undefined || seeded) {
      return;
    }
    seedFromView(view);
  }, [view, seeded, seedFromView]);

  // A configuration category is a deep-link target, for example
  // /config#experiments from the experiments monitor. React Router changes the
  // URL without scrolling, so the page scrolls the target into view once the
  // category is actually rendered.
  useEffect(() => {
    if (view === undefined) {
      return;
    }
    if (location.hash === '') {
      scrolledHash.current = '';
      return;
    }
    if (scrolledHash.current === location.hash) {
      return;
    }
    const targetID = location.hash.startsWith('#') ? location.hash.slice(1) : location.hash;
    const target = document.getElementById(targetID);
    if (target === null) {
      return;
    }
    scrolledHash.current = location.hash;
    target.scrollIntoView?.({ block: 'start' });
  }, [location.hash, view]);

  useEffect(() => {
    if (!save.isSuccess || save.data === undefined) {
      return;
    }
    // The submitted values are now the file values for these keys, so clear
    // their dirty state without waiting for the view refetch.
    if (save.variables !== undefined) {
      setSaved((previous) => ({ ...previous, ...save.variables?.patch }));
    }
    setSaveOutcome({ code: '', result: save.data });
    setSaveMessage('');
    setFieldErrors({});
  }, [save.isSuccess, save.data, save.variables]);

  // The refreshed view after a successful save carries the digest of the bytes
  // the process just wrote. Adopt it as soon as it lands, so the next save is
  // not rejected against our own write. This is the only refetch allowed to
  // move the seeded baseline: an external edit found by another refetch leaves
  // it where it is, and the next save then conflicts instead of silently
  // dropping the notice.
  useEffect(() => {
    if (view === undefined || pendingSaveBaseline.current === null) {
      return;
    }
    if (view.baseline === pendingSaveBaseline.current) {
      return;
    }
    pendingSaveBaseline.current = null;
    setSeededBaseline(view.baseline);
  }, [view]);

  useEffect(() => {
    if (!save.isError || save.error === null) {
      return;
    }
    const failure = configSaveFailure(save.error);
    setSaveMessage(userFacingConfigSaveError(save.error));
    setSaveOutcome(null);
    if (failure === undefined || view === undefined) {
      pendingSaveBaseline.current = null;
      setFieldErrors({});
      return;
    }
    const known = new Set(view.keys.map((key) => key.path));
    const errors: Record<string, string> = {};
    const extra: string[] = [];
    for (const field of failure.field_errors) {
      if (field.path !== '' && known.has(field.path)) {
        errors[field.path] = field.message;
      } else {
        extra.push(field.path === '' ? field.message : `${field.path}: ${field.message}`);
      }
    }
    setFieldErrors(errors);
    if (extra.length > 0) {
      setSaveMessage(`${userFacingConfigSaveError(save.error)}\n${extra.join('\n')}`);
    }
    if (configSaveDurabilityUnconfirmed(save.error)) {
      // The values are in force even though durability is unproven, so the
      // form must be clean; only the restart-recovery warning remains.
      if (save.variables !== undefined) {
        setSaved((previous) => ({ ...previous, ...save.variables?.patch }));
      }
      setSaveOutcome({
        code: failure.code,
        result: {
          applied: failure.applied,
          restart_required: failure.restart_required,
          persisted: failure.persisted,
        },
      });
      // The values are in force, so refresh the baseline the next save uses.
      // Refetch once per failed mutation: the refreshed view must not restart
      // the cycle that the error itself started.
      if (durabilityRefetched.current !== save.error) {
        durabilityRefetched.current = save.error;
        void refetch();
      }
    } else {
      // The save was rejected before it could rewrite the file, so the
      // baseline the form holds is still the one the file has.
      pendingSaveBaseline.current = null;
    }
  }, [save.isError, save.error, save.variables, view, refetch]);

  const pageReadOnly = view !== undefined && view.read_only_reason !== '';
  const patch: Record<string, string> = {};
  if (view !== undefined && !pageReadOnly) {
    for (const key of view.keys) {
      if (key.secret || key.read_only_reason !== '') {
        continue;
      }
      const draft = drafts[key.path];
      const current = saved[key.path];
      if (draft !== undefined && current !== undefined && draft !== current) {
        patch[key.path] = draft;
      }
    }
  }
  const changes = Object.entries(patch).map(([path, value]) => ({
    path,
    value,
    oldValue: saved[path] ?? '',
  }));
  const dangerousChanges = dangerousConfigChanges(changes.map((change) => change.path));
  const dirtyCount = changes.length;
  // Unit and duration syntax is checked before the request, with the same
  // syntax the backend accepts. An empty value is left to the backend because
  // whether it is allowed belongs to the key, not to its type.
  const clientErrors: Record<string, string> = {};
  if (view !== undefined) {
    for (const key of view.keys) {
      if (key.secret || key.read_only_reason !== '') {
        continue;
      }
      const message = configValueInputError(key.kind, drafts[key.path] ?? '');
      if (message !== undefined) {
        clientErrors[key.path] = message;
      }
    }
  }
  const hasClientErrors = Object.keys(clientErrors).length > 0;
  const canSave = view !== undefined && !pageReadOnly && dirtyCount > 0 && !hasClientErrors && !save.isPending && !reloading;
  const guard = useUnsavedChangesGuard(dirtyCount > 0);

  const performSave = () => {
    if (view === undefined || !canSave) {
      return;
    }
    setDangerOpen(false);
    setSaveOutcome(null);
    setSaveMessage('');
    setFieldErrors({});
    save.reset();
    // The seeded baseline, not the possibly newer view baseline: a stale draft
    // must conflict with an external edit the page never reseeded. The
    // baseline the write produces is adopted when its refetch lands.
    pendingSaveBaseline.current = seededBaseline;
    save.mutate({ baseline: seededBaseline, patch });
  };

  const requestSave = () => {
    if (!canSave) {
      return;
    }
    if (dangerousChanges.length > 0) {
      setDangerOpen(true);
      return;
    }
    performSave();
  };

  const reloadFromDisk = async () => {
    if (reloading) {
      return;
    }
    setReloading(true);
    // Dropping the failed mutation also clears the conflict notice; the user
    // chose to replace the local drafts with what is on disk.
    save.reset();
    setSaveMessage('');
    setSaveOutcome(null);
    setFieldErrors({});
    setDangerOpen(false);
    setRereadConfirmOpen(false);
    try {
      const result = await refetch();
      if (result.data !== undefined) {
        seedFromView(result.data);
      } else {
        setSaveMessage('重新读取配置失败，请稍后重试。');
      }
    } finally {
      setReloading(false);
    }
  };

  // Every entry that applies the disk values goes through the same reseed. An
  // explicit reread with unsaved drafts asks first: the drafts are the user's
  // work and reloading discards them, exactly like the conflict reload.
  const requestReloadFromDisk = () => {
    if (reloading) {
      return;
    }
    if (dirtyCount > 0) {
      setRereadConfirmOpen(true);
      return;
    }
    void reloadFromDisk();
  };

  const conflict = save.isError && configSaveFailure(save.error)?.code === 'conflict';

  const term = search.trim().toLowerCase();
  const filtered = term === '' || view === undefined
    ? view?.keys ?? []
    : view.keys.filter((key) => matchesSearch(key, term));
  const categories = groupConfigCategories(filtered);
  const categoryIDs = categories.map((category) => category.id).join('|');

  // Highlights the sidebar entry for whichever category is nearest the top
  // of the viewport, so scrolling through the page keeps the nav in sync
  // without requiring a click. IntersectionObserver is unavailable in some
  // test environments, so the effect is a no-op highlight-only enhancement.
  useEffect(() => {
    if (typeof IntersectionObserver === 'undefined' || categoryIDs === '') {
      return;
    }
    const elements = categoryIDs
      .split('|')
      .map((id) => document.getElementById(id))
      .filter((element): element is HTMLElement => element !== null);
    if (elements.length === 0) {
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries.filter((entry) => entry.isIntersecting);
        if (visible.length === 0) {
          return;
        }
        const topmost = visible.reduce((closest, entry) => (
          entry.boundingClientRect.top < closest.boundingClientRect.top ? entry : closest
        ));
        setActiveCategoryID(topmost.target.id);
      },
      { rootMargin: '-15% 0px -70% 0px', threshold: [0, 1] },
    );
    for (const element of elements) {
      observer.observe(element);
    }
    return () => observer.disconnect();
  }, [categoryIDs]);

  const scrollToCategory = (id: string) => {
    setActiveCategoryID(id);
    document.getElementById(id)?.scrollIntoView?.({ block: 'start' });
  };

  return (
    <div className={styles.page}>
      <section className={styles.heading} aria-labelledby="config-title">
        <div>
          <p className="eyebrow">设置</p>
          <h1 className={styles.title} id="config-title">配置</h1>
          <p className={styles.subtitle}>
            按配置节查看并编辑后台服务的配置项。被环境变量覆盖或偏离默认值的项会单独提示，其余项只显示当前值。
            保存时整体校验、写回配置文件，并把能立即生效的配置项应用到运行中的进程。
          </p>
        </div>
        <Badge size="lg" variant="light" color={pageReadOnly ? 'gray' : 'torrent'}>
          {pageReadOnly ? '只读' : '可编辑'}
        </Badge>
      </section>

      {query.isPending && (
        <div className={`panel panel--padding ${styles.loading}`} role="status" aria-busy="true" aria-label="正在读取配置">
          <Loader size="sm" />
          <span>正在读取配置…</span>
        </div>
      )}

      {query.isError && query.data === undefined && !query.isPending && (
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} title="无法读取配置" className={styles.alert}>
          <Group justify="space-between" gap="sm">
            <span>后台服务暂时没有返回配置视图，请稍后重试。</span>
            <Button variant="light" color="torrent" size="compact-sm" onClick={() => void query.refetch()}>
              重新读取
            </Button>
          </Group>
        </Alert>
      )}

      {view !== undefined && (
        <>
          <ReadOnlyBanner view={view} />

          {view.config_file !== '' && (
            <ReloadStatus
              view={view}
              onReread={requestReloadFromDisk}
              rereadDisabled={save.isPending || reloading}
            />
          )}

          {saveOutcome === null && <SaveStatus view={view} />}

          {saveOutcome !== null && (
            <Alert
              color={saveOutcome.code === 'durability_unconfirmed' ? 'yellow' : 'green'}
              icon={saveOutcome.code === 'durability_unconfirmed'
                ? <IconAlertTriangle size={16} />
                : <IconDeviceFloppy size={16} />}
              title={saveOutcome.code === 'durability_unconfirmed' ? '新配置已生效，但持久化未确认' : '保存成功'}
              className={styles.alert}
            >
              {saveOutcome.code === 'durability_unconfirmed' && (
                <p className={styles.alertText}>{saveMessage}</p>
              )}
              <SaveResultGroups result={saveOutcome.result} />
            </Alert>
          )}

          {saveMessage !== '' && saveOutcome === null && (
            <Alert color={conflict ? 'yellow' : 'red'} icon={<IconAlertTriangle size={16} />} title="保存未完成" className={styles.alert}>
              <p className={styles.alertText}>{saveMessage}</p>
              {conflict && (
                <Group justify="space-between" align="center" gap="sm" mt="sm" wrap="wrap">
                  <span className={styles.alertHint}>
                    重新加载会放弃页面上的本地改动，并读取磁盘上的最新配置。
                  </span>
                  <Button
                    variant="light"
                    color="yellow"
                    size="compact-sm"
                    onClick={() => void reloadFromDisk()}
                    loading={reloading}
                  >
                    重新加载配置
                  </Button>
                </Group>
              )}
            </Alert>
          )}

          <div className={styles.toolbar}>
            <TextInput
              aria-label="搜索配置项"
              placeholder="搜索配置项、说明、来源或值"
              leftSection={<IconSearch size={16} />}
              value={search}
              onChange={(event) => setSearch(event.currentTarget.value)}
              className={styles.search}
            />
            <Group gap="sm" className={styles.actions}>
              {view.config_file !== '' && (
                <span className={styles.file}>
                  配置文件：<code className={styles.code}>{view.config_file}</code>
                </span>
              )}
              {!pageReadOnly && (
                <>
                  {dirtyCount > 0 && (
                    <Badge variant="light" color="yellow" data-testid="config-dirty-count">
                      未保存 {dirtyCount} 项
                    </Badge>
                  )}
                  <Button
                    color="torrent"
                    leftSection={<IconDeviceFloppy size={16} />}
                    onClick={requestSave}
                    loading={save.isPending}
                    disabled={!canSave}
                  >
                    保存
                  </Button>
                </>
              )}
            </Group>
          </div>

          {changes.length > 0 && (
            <section className={`panel panel--padding ${styles.diff}`} data-testid="config-diff-preview" aria-label="改动预览">
              <div className={styles.diffHeader}>
                <div>
                  <p className={styles.diffTitle}>改动预览</p>
                  <p className={styles.diffHint}>
                    保存后将写入以下 {changes.length} 项改动；红色删除线为旧值，仅用于对照，不会写入配置文件。
                  </p>
                </div>
                <Badge variant="light" color="yellow">{changes.length} 项</Badge>
              </div>
              <ul className={styles.diffList}>
                {changes.map((change) => {
                  const dangerous = dangerousConfigKey(change.path);
                  return (
                    <li key={change.path} className={styles.diffItem}>
                      <code className={styles.code}>{change.path}</code>
                      <span className={styles.diffValues}>
                        <code className={`${styles.diffValue} ${styles.diffValueOld}`}>
                          <span aria-hidden="true">− </span>
                          {change.oldValue === '' ? '（空）' : change.oldValue}
                        </code>
                        <span className={styles.diffArrow} aria-hidden="true">→</span>
                        <code className={`${styles.diffValue} ${styles.diffValueNew}`}>
                          <span aria-hidden="true">+ </span>
                          {change.value === '' ? '（空）' : change.value}
                        </code>
                      </span>
                      {dangerous !== undefined && (
                        <Badge variant="light" color="red">{dangerousCategoryLabel(dangerous.category)}</Badge>
                      )}
                    </li>
                  );
                })}
              </ul>
            </section>
          )}

          {term !== '' && filtered.length === 0 && (
            <div className={`panel panel--padding ${styles.empty}`} role="status">
              没有匹配的配置项
            </div>
          )}

          <div className={styles.layout}>
            <nav className={styles.nav} aria-label="配置分类导航" data-testid="config-nav">
              {categories.map((category) => (
                <button
                  key={category.id}
                  type="button"
                  className={
                    category.id === activeCategoryID
                      ? `${styles.navItem} ${styles.navItemActive}`
                      : styles.navItem
                  }
                  aria-current={category.id === activeCategoryID ? 'true' : undefined}
                  data-testid={`config-nav-${category.id}`}
                  onClick={() => scrollToCategory(category.id)}
                >
                  <span className={styles.navItemLabel}>{category.label}</span>
                  <Badge variant="light" color="gray" size="sm">{categoryKeyCount(category)}</Badge>
                </button>
              ))}
            </nav>

            <div className={styles.content} data-testid="config-content">
              {categories.map((category) => (
                <section
                  key={category.id}
                  id={category.id}
                  className={styles.section}
                  data-testid={`config-category-${category.id}`}
                  aria-labelledby={`config-category-heading-${category.id}`}
                >
                  <div className={styles.sectionHeader}>
                    <span className={styles.sectionHeading}>
                      <h2 className={styles.sectionTitle} id={`config-category-heading-${category.id}`}>
                        {category.label}
                      </h2>
                      {category.description !== '' && (
                        <span className={styles.sectionDescription}>{category.description}</span>
                      )}
                    </span>
                    <Badge variant="light" color="gray">{categoryKeyCount(category)} 项</Badge>
                  </div>
                  {category.sections.map((section) => (
                    <section
                      key={section.id}
                      id={section.id === category.id ? undefined : section.id}
                      className={styles.subsection}
                      aria-label={section.label}
                    >
                      {section.id !== category.id && (
                        <h3 className={styles.subsectionTitle}>{section.label}</h3>
                      )}
                      <div className={styles.keys}>
                        {section.keys.map((key) => (
                          <KeyCard
                            key={key.path}
                            configKey={key}
                            draft={drafts[key.path] ?? ''}
                            editable={!pageReadOnly && key.read_only_reason === ''}
                            onChange={(value) => setDrafts((previous) => ({ ...previous, [key.path]: value }))}
                            error={fieldErrors[key.path] ?? clientErrors[key.path]}
                          />
                        ))}
                      </div>
                    </section>
                  ))}
                </section>
              ))}
            </div>
          </div>

          <DangerousSaveDialog
            opened={dangerOpen}
            changes={dangerousChanges}
            onCancel={() => setDangerOpen(false)}
            onConfirm={performSave}
          />

          <Modal
            opened={rereadConfirmOpen}
            onClose={() => setRereadConfirmOpen(false)}
            title="重新读取配置？"
            centered
            closeButtonProps={{ 'aria-label': '关闭弹窗' }}
          >
            <Stack gap="sm">
              <p className={styles.modalText}>
                有 {dirtyCount} 项未保存的改动。重新读取会放弃这些改动，并读取磁盘上的最新配置。
              </p>
              <Group justify="flex-end" gap="sm">
                <Button variant="default" onClick={() => setRereadConfirmOpen(false)}>留在此页</Button>
                <Button color="yellow" onClick={() => void reloadFromDisk()}>放弃改动并重新读取</Button>
              </Group>
            </Stack>
          </Modal>

          <Modal
            opened={guard.pendingHref !== null}
            onClose={guard.stay}
            title="离开配置页面？"
            centered
            closeButtonProps={{ 'aria-label': '关闭弹窗' }}
          >
            <Stack gap="sm">
              <p className={styles.modalText}>
                有 {dirtyCount} 项未保存的改动，离开后这些改动会丢失。
              </p>
              <Group justify="flex-end" gap="sm">
                <Button variant="default" onClick={guard.stay}>留在此页</Button>
                <Button color="red" onClick={guard.leave}>放弃改动并离开</Button>
              </Group>
            </Stack>
          </Modal>
        </>
      )}
    </div>
  );
}
