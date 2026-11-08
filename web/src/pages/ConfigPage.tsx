import { Accordion, Alert, Badge, Button, Code, Group, Loader, TextInput } from '@mantine/core';
import { IconAlertTriangle, IconInfoCircle, IconSearch } from '@tabler/icons-react';
import { useState } from 'react';
import { useAuth } from '../app/auth-context';
import { useConfig } from '../queries/hooks';
import type { ConfigKey, ConfigView } from '../types/api';
import { usePageVisible } from '../utils/visibility';
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
  experiments: '实验',
  'experiments.upload': '上传实验',
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

/**
 * Groups the keys by configuration section, keeping the schema order of the
 * first key of every section.
 */
function groupConfigKeys(keys: ConfigKey[]): ConfigSection[] {
  const sections: ConfigSection[] = [];
  const byID = new Map<string, ConfigSection>();
  for (const key of keys) {
    const id = key.section === '' ? 'root' : key.section;
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

function KeyCard({ configKey }: { configKey: ConfigKey }) {
  return (
    <article className={`panel panel--padding ${styles.key}`} aria-label={configKey.path}>
      <div className={styles.keyHeader}>
        <code className={styles.path}>{configKey.path}</code>
        <Group gap={6} wrap="wrap">
          <Badge variant="outline" color="gray">{kindLabel(configKey.kind)}</Badge>
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
        <p className={styles.documentation}>{configKey.documentation}</p>
      )}

      <dl className={styles.values}>
        <div>
          <dt>生效值</dt>
          <dd data-testid="config-effective-value">{displayValue(configKey, configKey.effective_value)}</dd>
        </div>
        <div>
          <dt>文件值</dt>
          <dd data-testid="config-file-value">{displayValue(configKey, configKey.file_value)}</dd>
        </div>
        <div>
          <dt>默认值</dt>
          <dd data-testid="config-default-value">{displayValue(configKey, configKey.default)}</dd>
        </div>
        <div>
          <dt>来源</dt>
          <dd data-testid="config-source">{sourceLabel(configKey.source)}</dd>
        </div>
      </dl>

      {configKey.values.length > 0 && (
        <p className={styles.meta}>可选值：{configKey.values.join(' / ')}</p>
      )}
      {configKey.component !== '' && (
        <p className={styles.meta}>所属组件：{componentLabel(configKey.component)}</p>
      )}
      {configKey.read_only_reason === 'environment_override' && (
        <p className={styles.readOnlyNote}>
          该项由环境变量 <code className={styles.code}>{configKey.environment}</code> 覆盖，配置文件中的值不会生效。
        </p>
      )}
      {configKey.read_only_reason === 'secret' && (
        <p className={styles.readOnlyNote}>
          口令属于凭据，不会在页面上显示，也不能在这里编辑；保存其他配置时它会原样保留。
        </p>
      )}
    </article>
  );
}

export function ConfigPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const query = useConfig(auth.api, auth.isReady && visible);
  const [search, setSearch] = useState('');

  const view = query.data;
  const term = search.trim().toLowerCase();
  const filtered = term === '' || view === undefined
    ? view?.keys ?? []
    : view.keys.filter((key) => matchesSearch(key, term));
  const sections = groupConfigKeys(filtered);

  return (
    <div className={styles.page}>
      <section className={styles.heading} aria-labelledby="config-title">
        <div>
          <p className="eyebrow">设置</p>
          <h1 className={styles.title} id="config-title">配置</h1>
          <p className={styles.subtitle}>
            按配置节浏览后台服务的全部配置项：文件值、生效值、来源与生效方式一目了然。
            当前为只读视图，保存与热应用将在后续版本提供。
          </p>
        </div>
        <Badge size="lg" variant="light" color="torrent">只读</Badge>
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

          <div className={styles.toolbar}>
            <TextInput
              aria-label="搜索配置项"
              placeholder="搜索配置项、说明、来源或值"
              leftSection={<IconSearch size={16} />}
              value={search}
              onChange={(event) => setSearch(event.currentTarget.value)}
              className={styles.search}
            />
            {view.config_file !== '' && (
              <span className={styles.file}>
                配置文件：<code className={styles.code}>{view.config_file}</code>
              </span>
            )}
          </div>

          {term !== '' && filtered.length === 0 && (
            <div className={`panel panel--padding ${styles.empty}`} role="status">
              没有匹配的配置项
            </div>
          )}

          <Accordion
            multiple
            variant="separated"
            defaultValue={sections.map((section) => section.id)}
            className={styles.accordion}
          >
            {sections.map((section) => (
              <Accordion.Item key={section.id} value={section.id} className={styles.section}>
                <Accordion.Control>
                  <Group justify="space-between" pr="md" wrap="nowrap">
                    <span className={styles.sectionTitle}>{section.label}</span>
                    <Badge variant="light" color="gray">{section.keys.length} 项</Badge>
                  </Group>
                </Accordion.Control>
                <Accordion.Panel>
                  <div className={styles.keys}>
                    {section.keys.map((key) => (
                      <KeyCard key={key.path} configKey={key} />
                    ))}
                  </div>
                </Accordion.Panel>
              </Accordion.Item>
            ))}
          </Accordion>
        </>
      )}
    </div>
  );
}
