import { Badge, Button, Group, Skeleton } from '@mantine/core';
import { IconAdjustments, IconFlask } from '@tabler/icons-react';
import { Link } from 'react-router-dom';
import type { useExperiment } from '../../queries/hooks';
import { formatBytes } from '../../utils/format';
import {
  formatPeerSampleInterval,
  formatSignedBytes,
  experimentModeLabel,
  experimentModeOption,
} from '../../utils/experiments';
import { formatByteQuantity, formatUploadRate } from '../../utils/quantities';
import { userFacingError } from '../../utils/user-facing-error';
import styles from './ExperimentSummaryCard.module.css';

type ExperimentQuery = ReturnType<typeof useExperiment>;

/**
 * Dashboard summary of the always-on synthetic-upload experiment: the running
 * settings beside the live real-versus-reported counters. This replaces the
 * former dedicated /experiments monitor page; experiment settings are edited
 * on the configuration page's "实验" category, linked from the button below.
 *
 * Unlike the former page's card, this one is not itself a link — it carries
 * its own "修改实验设置" action instead, so a nested interactive element never
 * ends up inside another one.
 */
export function ExperimentSummaryCard({ query }: { query: ExperimentQuery }) {
  const view = query.data;

  if (view === undefined && query.isPending) {
    return (
      <div
        className={`panel ${styles.card} ${styles.loading}`}
        role="status"
        aria-busy="true"
        aria-label="正在读取上传实验状态"
        data-testid="experiment-summary-card"
      >
        <Skeleton height={112} radius="md" />
      </div>
    );
  }

  if (view === undefined) {
    if (query.isError) {
      return (
        <div className={`panel ${styles.card} ${styles.unavailable}`} role="status" data-testid="experiment-summary-card">
          <strong>上传实验状态暂不可用</strong>
          <span>{userFacingError(query.error, '后台服务暂时没有返回实验状态，请稍后重试。')}</span>
        </div>
      );
    }
    return null;
  }

  const settings = view.settings;
  const modeDescription = experimentModeOption(settings.mode)?.description;
  const overreported = view.totals.overreported_bytes;
  const tone = overreported > 0 ? styles.surplus : overreported < 0 ? styles.shortfall : undefined;

  return (
    <div className={`panel ${styles.card}`} aria-label="上传实验" data-testid="experiment-summary-card">
      <div className={styles.heading}>
        <div>
          <p className="eyebrow">反作弊测试</p>
          <h2 className={styles.title}><IconFlask size={17} aria-hidden="true" /> 上传实验</h2>
        </div>
        <Group gap="xs" align="center" wrap="nowrap">
          <Badge variant="light" color="torrent">{experimentModeLabel(settings.mode)}</Badge>
          <Button
            component={Link}
            to="/config#experiments"
            variant="light"
            color="torrent"
            size="compact-sm"
            leftSection={<IconAdjustments size={15} />}
          >
            修改实验设置
          </Button>
        </Group>
      </div>

      {query.isError && (
        <div className="refresh-warning" role="alert">
          <span aria-hidden="true">!</span>
          <span>最新实验状态刷新失败，当前显示上一次有效快照。</span>
        </div>
      )}

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

      <dl className={styles.settingsList}>
        {settings.mode === 'inject' && (
          <div>
            <dt>虚报速率</dt>
            <dd>{formatUploadRate(settings.rate_bytes_per_second) ?? '—'}</dd>
          </div>
        )}
        {settings.mode === 'amplify' && (
          <div>
            <dt>放大倍率</dt>
            <dd>×{settings.multiplier}</dd>
          </div>
        )}
        <div>
          <dt>抖动百分比</dt>
          <dd>{settings.jitter_percent}%</dd>
        </div>
        <div>
          <dt>上报上限</dt>
          <dd>{settings.cap_bytes > 0 ? (formatByteQuantity(settings.cap_bytes) ?? '—') : '不封顶'}</dd>
        </div>
        <div>
          <dt>随机种子</dt>
          <dd>{view.seed}</dd>
        </div>
        <div>
          <dt>Announce 日志</dt>
          <dd>{view.announce_log !== '' ? view.announce_log : '未开启'}</dd>
        </div>
        <div>
          <dt>Peer 采样间隔</dt>
          <dd>{formatPeerSampleInterval(view.peer_sample_interval_seconds)}</dd>
        </div>
      </dl>
      {modeDescription !== undefined && <p className={styles.note}>{modeDescription}</p>}
    </div>
  );
}
