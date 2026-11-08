/**
 * Impact categories of the configuration keys that can make the daemon harder
 * to reach or change the shape of its output after a restart.
 */
export type DangerousConfigCategory = 'availability' | 'output' | 'mount';

export interface DangerousConfigKey {
  path: string;
  category: DangerousConfigCategory;
  /** The user-visible consequence of changing the key. */
  impact: string;
}

/**
 * The configuration keys whose change needs a second confirmation. The list is
 * fixed here because the API view does not (yet) carry a danger marker; keep it
 * in sync with the keys named in the ticket.
 */
const DANGEROUS_CONFIG: Record<string, Omit<DangerousConfigKey, 'path'>> = {
  'http.listen_addr': {
    category: 'availability',
    impact: '重启后 HTTP 服务会在新地址监听；旧地址不再可访问，你可能因此失去对配置页面的访问。',
  },
  'http.auth.enabled': {
    category: 'availability',
    impact: '重启后 HTTP 认证开关改变：关闭后管理接口可能对网络内访客开放，开启后需要重新登录。',
  },
  backend: {
    category: 'output',
    impact: '重启后输出后端切换（fuse / smb / none）；当前挂载点会消失或文件树形态改变，正在播放的路径可能失效。',
  },
  'smb.listen_addr': {
    category: 'availability',
    impact: '重启后 SMB 服务会在新地址监听；旧的共享地址不再可用。',
  },
  'smb.strm': {
    category: 'output',
    impact: '重启后 SMB 输出改为 STRM 文件形态；播放器拿到的文件内容与播放方式会改变。',
  },
  'fuse.mountpoint': {
    category: 'mount',
    impact: '重启后 FUSE 挂载点改变；旧挂载点不再提供文件，依赖该路径的播放器与脚本会失效。',
  },
  'fuse.strm_mountpoint': {
    category: 'mount',
    impact: '重启后 STRM 挂载点改变；指向旧路径的媒体库会找不到文件。',
  },
  'connections.listen_port': {
    category: 'availability',
    impact: '重启后 peer 监听端口改变；需要同步调整防火墙与端口映射，否则可能影响连接与做种。',
  },
};

const CATEGORY_LABELS: Record<DangerousConfigCategory, string> = {
  availability: '可能失联',
  output: '输出形态变化',
  mount: '挂载变化',
};

/** The Chinese label of an impact category, shown next to the diff and in the confirmation. */
export function dangerousCategoryLabel(category: DangerousConfigCategory): string {
  return CATEGORY_LABELS[category];
}

/** Returns the dangerous-key metadata for one configuration path, if any. */
export function dangerousConfigKey(path: string): DangerousConfigKey | undefined {
  const entry = DANGEROUS_CONFIG[path];
  if (entry === undefined) {
    return undefined;
  }
  return { path, ...entry };
}

/**
 * Filters the changed configuration paths to the dangerous ones, keeping the
 * order in which the page presents the changes.
 */
export function dangerousConfigChanges(paths: readonly string[]): DangerousConfigKey[] {
  const changes: DangerousConfigKey[] = [];
  for (const path of paths) {
    const change = dangerousConfigKey(path);
    if (change !== undefined) {
      changes.push(change);
    }
  }
  return changes;
}
