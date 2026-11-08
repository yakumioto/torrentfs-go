import { Badge } from '@mantine/core';
import { IconChevronRight, IconFlask } from '@tabler/icons-react';
import { Link } from 'react-router-dom';
import type { LabView } from '../../types/api';
import { formatBytes } from '../../utils/format';
import {
  formatPeerSampleInterval,
  formatSignedBytes,
  labModeLabel,
} from '../../utils/lab';
import styles from './LabSummaryCard.module.css';

/**
 * Dashboard summary of the always-on synthetic-upload experiment. It is a link
 * to the dedicated /experiments page, which shows the running settings beside
 * the live counters.
 *
 * The card deliberately shows both points in time: the live real counter and
 * the counter captured at the last announce. Reported and overreported values
 * are only comparable against the latter; the difference to the live counter
 * is the pending real upload that the tracker has not seen yet.
 */
export function LabSummaryCard({ view }: { view: LabView }) {
  const overreported = view.totals.overreported_bytes;
  const tone = overreported > 0 ? styles.surplus : overreported < 0 ? styles.shortfall : undefined;

  return (
    <Link to="/experiments" className={`panel ${styles.card}`} aria-label="上传实验：打开上传实验监控">
      <div className={styles.heading}>
        <div>
          <p className="eyebrow">反作弊测试</p>
          <h2 className={styles.title}><IconFlask size={17} aria-hidden="true" /> 上传实验</h2>
        </div>
        <Badge variant="light" color="torrent">{labModeLabel(view.settings?.mode)}</Badge>
      </div>
      <dl className={styles.stats}>
        <div>
          <dt>实时真实上传</dt>
          <dd>{formatBytes(view.totals.real_uploaded_bytes)}</dd>
        </div>
        <div>
          <dt>上次上报时真实</dt>
          <dd>{formatBytes(view.totals.real_uploaded_at_last_announce_bytes)}</dd>
        </div>
        <div>
          <dt>Tracker 上报</dt>
          <dd>{formatBytes(view.totals.reported_uploaded_bytes)}</dd>
        </div>
        <div>
          <dt>虚报量</dt>
          <dd className={tone}>{formatSignedBytes(overreported)}</dd>
        </div>
        <div>
          <dt>待上报真实增量</dt>
          <dd>{formatBytes(view.totals.pending_real_upload_bytes)}</dd>
        </div>
      </dl>
      <p className={styles.note}>
        上报值只在 tracker announce 时更新，两次 announce 之间实时真实量会领先于上报值；虚报量按上次上报时点对比。
      </p>
      <div className={styles.meta}>
        <span>随机种子 {view.seed}</span>
        <span>每 {formatPeerSampleInterval(view.peer_sample_interval_seconds)}采样 peer</span>
        <span className={styles.more}>查看监控<IconChevronRight size={14} aria-hidden="true" /></span>
      </div>
    </Link>
  );
}
