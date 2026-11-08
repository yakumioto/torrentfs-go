import {
  Accordion,
  Alert,
  Badge,
  Button,
  Code,
  Group,
  Loader,
  NativeSelect,
  NumberInput,
  Switch,
  TextInput,
} from '@mantine/core';
import {
  IconAlertTriangle,
  IconDeviceFloppy,
  IconInfoCircle,
  IconSearch,
} from '@tabler/icons-react';
import { useEffect, useRef, useState } from 'react';
import { useAuth } from '../app/auth-context';
import { useConfig, useSaveConfig } from '../queries/hooks';
import type { ConfigKey, ConfigSaveResult, ConfigView } from '../types/api';
import { usePageVisible } from '../utils/visibility';
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
        <p className={styles.meta}>可选值：{configKey.values.filter((value) => value !== '').join(' / ')}</p>
      )}
      {configKey.component !== '' && (
        <p className={styles.meta}>所属组件：{componentLabel(configKey.component)}</p>
      )}

      {!configKey.secret && (
        <div className={styles.editor}>
          <span className={styles.editorLabel}>新值</span>
          <KeyControl configKey={configKey} draft={draft} editable={editable} onChange={onChange} />
        </div>
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
      {result.applied.length > 0 && (
        <div>
          <strong>已生效</strong>
          <ul className={styles.resultKeys} data-testid="config-applied-keys">
            {result.applied.map((path) => <li key={path}><code className={styles.code}>{path}</code></li>)}
          </ul>
        </div>
      )}
      {result.restart_required.length > 0 && (
        <div>
          <strong>重启后生效</strong>
          <ul className={styles.resultKeys} data-testid="config-restart-keys">
            {result.restart_required.map((path) => <li key={path}><code className={styles.code}>{path}</code></li>)}
          </ul>
        </div>
      )}
    </div>
  );
}

interface SaveOutcome {
  code: string;
  result: ConfigSaveResult;
}

export function ConfigPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const query = useConfig(auth.api, auth.isReady && visible);
  const save = useSaveConfig(auth.api);

  const [search, setSearch] = useState('');
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [seeded, setSeeded] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [saveOutcome, setSaveOutcome] = useState<SaveOutcome | null>(null);
  const [saveMessage, setSaveMessage] = useState('');
  const durabilityRefetched = useRef<unknown>(null);

  const view = query.data;
  const refetch = query.refetch;

  // Seed the form from the file layer once. A background refetch must not
  // replace what the user is editing; a save deliberately keeps the draft the
  // user submitted.
  useEffect(() => {
    if (view === undefined || seeded) {
      return;
    }
    const next: Record<string, string> = {};
    for (const key of view.keys) {
      next[key.path] = key.file_value;
    }
    setDrafts(next);
    setSeeded(true);
  }, [view, seeded]);

  useEffect(() => {
    if (!save.isSuccess || save.data === undefined) {
      return;
    }
    setSaveOutcome({ code: '', result: save.data });
    setSaveMessage('');
    setFieldErrors({});
  }, [save.isSuccess, save.data]);

  useEffect(() => {
    if (!save.isError || save.error === null) {
      return;
    }
    const failure = configSaveFailure(save.error);
    setSaveMessage(userFacingConfigSaveError(save.error));
    setSaveOutcome(null);
    if (failure === undefined || view === undefined) {
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
    }
  }, [save.isError, save.error, view, refetch]);

  const pageReadOnly = view !== undefined && view.read_only_reason !== '';
  const patch: Record<string, string> = {};
  if (view !== undefined && !pageReadOnly) {
    for (const key of view.keys) {
      if (key.secret || key.read_only_reason !== '') {
        continue;
      }
      const draft = drafts[key.path];
      if (draft !== undefined && draft !== key.file_value) {
        patch[key.path] = draft;
      }
    }
  }
  const dirtyCount = Object.keys(patch).length;
  const canSave = view !== undefined && !pageReadOnly && dirtyCount > 0 && !save.isPending;

  const submit = () => {
    if (view === undefined || !canSave) {
      return;
    }
    setSaveOutcome(null);
    setSaveMessage('');
    setFieldErrors({});
    save.reset();
    save.mutate({ baseline: view.baseline, patch });
  };

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
            按配置节查看并编辑后台服务的配置项：文件值、生效值、来源与生效方式一目了然。
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
            <Alert color="red" icon={<IconAlertTriangle size={16} />} title="保存未完成" className={styles.alert}>
              <p className={styles.alertText}>{saveMessage}</p>
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
                    onClick={submit}
                    loading={save.isPending}
                    disabled={!canSave}
                  >
                    保存
                  </Button>
                </>
              )}
            </Group>
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
                      <KeyCard
                        key={key.path}
                        configKey={key}
                        draft={drafts[key.path] ?? ''}
                        editable={!pageReadOnly && key.read_only_reason === ''}
                        onChange={(value) => setDrafts((previous) => ({ ...previous, [key.path]: value }))}
                        error={fieldErrors[key.path]}
                      />
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
