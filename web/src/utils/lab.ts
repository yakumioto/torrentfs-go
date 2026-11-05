import type { LabMode, LabTorrentStat, LabView } from '../types/api';
import { formatBytes } from './format';

export interface LabModeOption {
  value: LabMode;
  label: string;
  description: string;
}

/**
 * The three arms of the experiment. The copy has to make the traffic model
 * explicit, because which one is active decides whether the reported upload is
 * related to real peer traffic at all.
 */
export const LAB_MODE_OPTIONS: readonly LabModeOption[] = [
  {
    value: 'baseline',
    label: '基线（真实上报）',
    description: '不修改上报量，只记录真实上报与 peer 采样，作为对照组。',
  },
  {
    value: 'inject',
    label: '注入虚报',
    description: '按固定速率随时间虚增上报量，与真实传输无关，用于验证 tracker 能否识别异常。',
  },
  {
    value: 'amplify',
    label: '放大真实上传',
    description: '按倍率放大真实传输量，只有真实上传才会增长，模拟更隐蔽的作弊。',
  },
];

export function labModeOption(mode: LabMode | '' | undefined): LabModeOption | undefined {
  return LAB_MODE_OPTIONS.find((option) => option.value === mode);
}

export function labModeLabel(mode: LabMode | '' | undefined): string {
  return labModeOption(mode)?.label ?? '未启用';
}

/**
 * Reported minus real upload. The contract allows this to be negative: an
 * inject-mode restart can report less than the session's real counter.
 */
export function labOverreportedBytes(reported: number, real: number): number {
  return reported - real;
}

/**
 * Formats a diff with an explicit sign so a shortfall is never mistaken for a
 * surplus. Uses the same byte units as the rest of the UI.
 */
export function formatSignedBytes(value: number): string {
  if (!Number.isFinite(value)) {
    return '—';
  }
  if (value === 0) {
    return formatBytes(0);
  }
  return `${value > 0 ? '+' : '-'}${formatBytes(Math.abs(value))}`;
}

export function labTorrentStat(view: LabView | undefined, infoHash: string): LabTorrentStat | undefined {
  if (view === undefined || !view.enabled || infoHash === '') {
    return undefined;
  }
  return view.torrents.find((stat) => stat.info_hash === infoHash);
}

/** Indexes per-torrent lab counters by info hash for the task list. */
export function indexLabTorrentStats(view: LabView | undefined): Map<string, LabTorrentStat> {
  if (view === undefined || !view.enabled) {
    return new Map();
  }
  return new Map(view.torrents.map((stat) => [stat.info_hash, stat]));
}

/** Human-readable peer sampling period; the daemon reports whole seconds. */
export function formatPeerSampleInterval(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return '—';
  }
  return `${seconds} 秒`;
}
