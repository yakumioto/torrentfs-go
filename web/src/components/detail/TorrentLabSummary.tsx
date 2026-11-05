import { Badge } from '@mantine/core';
import { IconFlask } from '@tabler/icons-react';
import type { LabView } from '../../types/api';
import { formatBytes, formatDate } from '../../utils/format';
import {
  formatPeerSampleInterval,
  formatRelativeTime,
  formatSignedBytes,
  labModeLabel,
  labOverreportedBytes,
  labPendingRealUploadBytes,
  labTorrentStat,
} from '../../utils/lab';
import styles from './TorrentLabSummary.module.css';

/**
 * Per-torrent view of the synthetic-upload experiment. Renders nothing unless
 * the daemon reports the experiment as enabled, so normal deployments keep the
 * detail page unchanged.
 *
 * Reported and overreported values use the real counter captured at the last
 * announce; the live counter and the pending delta are shown separately so a
 * large gap is explained rather than read as a broken multiplier.
 */
export function TorrentLabSummary({ lab, infoHash }: { lab?: LabView; infoHash: string }) {
  if (lab?.enabled !== true) {
    return null;
  }
  const stat = labTorrentStat(lab, infoHash);
  const overreported = stat === undefined
    ? undefined
    : labOverreportedBytes(stat.reported_uploaded_bytes, stat.real_uploaded_at_last_announce_bytes);
  const pending = stat === undefined
    ? undefined
    : labPendingRealUploadBytes(stat.real_uploaded_bytes, stat.real_uploaded_at_last_announce_bytes);
  const tone = overreported === undefined
    ? undefined
    : overreported > 0 ? styles.surplus : overreported < 0 ? styles.shortfall : undefined;

  return (
    <section className={`${styles.panel} panel panel--padding`} aria-labelledby="torrent-lab-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">反作弊测试</p>
          <h2 id="torrent-lab-title" className={styles.title}>
            <IconFlask size={17} aria-hidden="true" /> 上传实验
          </h2>
        </div>
        <Badge variant="light" color="torrent">{labModeLabel(lab.settings?.mode)}</Badge>
      </div>
      <dl className={styles.grid}>
        <div><dt>实时真实上传</dt><dd>{stat === undefined ? '—' : formatBytes(stat.real_uploaded_bytes)}</dd></div>
        <div><dt>上次上报时真实</dt><dd>{stat === undefined ? '—' : formatBytes(stat.real_uploaded_at_last_announce_bytes)}</dd></div>
        <div><dt>Tracker 上报</dt><dd>{stat === undefined ? '—' : formatBytes(stat.reported_uploaded_bytes)}</dd></div>
        <div><dt>虚报量</dt><dd className={tone}>{overreported === undefined ? '—' : formatSignedBytes(overreported)}</dd></div>
        <div><dt>待上报真实增量</dt><dd>{pending === undefined ? '—' : formatBytes(pending)}</dd></div>
        <div>
          <dt>上次 announce 时间</dt>
          <dd>{stat === undefined ? '—' : formatRelativeTime(stat.last_announce_at)}</dd>
        </div>
      </dl>
      <p className={styles.note}>
        Tracker 只在上报（announce）时看到新值，两次 announce 之间实时真实上传会领先于上报值；
        虚报量按上次上报时点对比，待上报真实增量会在下次 announce 时按实验模式计入上报。
        本轮实验的 peer 采样间隔为 {formatPeerSampleInterval(lab.peer_sample_interval_seconds)}。
      </p>
      {stat !== undefined && stat.last_announce_at !== null && stat.last_announce_at !== undefined && (
        <p className={styles.note}>上次 announce 绝对时间：{formatDate(stat.last_announce_at)}</p>
      )}
    </section>
  );
}
