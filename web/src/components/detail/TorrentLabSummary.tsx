import { Badge } from '@mantine/core';
import { IconFlask } from '@tabler/icons-react';
import type { LabView } from '../../types/api';
import { formatBytes } from '../../utils/format';
import {
  formatPeerSampleInterval,
  formatSignedBytes,
  labModeLabel,
  labOverreportedBytes,
  labTorrentStat,
} from '../../utils/lab';
import styles from './TorrentLabSummary.module.css';

/**
 * Per-torrent view of the synthetic-upload experiment. Renders nothing unless
 * the daemon reports the experiment as enabled, so normal deployments keep the
 * detail page unchanged.
 */
export function TorrentLabSummary({ lab, infoHash }: { lab?: LabView; infoHash: string }) {
  if (lab?.enabled !== true) {
    return null;
  }
  const stat = labTorrentStat(lab, infoHash);
  const overreported = stat === undefined
    ? undefined
    : labOverreportedBytes(stat.reported_uploaded_bytes, stat.real_uploaded_bytes);
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
        <div><dt>真实上传</dt><dd>{stat === undefined ? '—' : formatBytes(stat.real_uploaded_bytes)}</dd></div>
        <div><dt>Tracker 上报</dt><dd>{stat === undefined ? '—' : formatBytes(stat.reported_uploaded_bytes)}</dd></div>
        <div><dt>虚报量</dt><dd className={tone}>{overreported === undefined ? '—' : formatSignedBytes(overreported)}</dd></div>
      </dl>
      <p className={styles.note}>
        本轮实验的 peer 采样间隔为 {formatPeerSampleInterval(lab.peer_sample_interval_seconds)}。
        {stat === undefined
          ? ' 这个种子还没有向 tracker 上报过上传量，首次 announce 后这里会出现数据。'
          : ''}
      </p>
    </section>
  );
}
