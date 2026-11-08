import { Alert, Badge, Button, Group, Loader } from '@mantine/core';
import { IconAdjustments, IconAlertTriangle, IconFlask, IconRefresh } from '@tabler/icons-react';
import { Link } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import { useExperiment } from '../queries/hooks';
import { formatBytes } from '../utils/format';
import {
  formatPeerSampleInterval,
  formatSignedBytes,
  experimentModeLabel,
  experimentModeOption,
} from '../utils/experiments';
import { formatByteQuantity, formatUploadRate } from '../utils/quantities';
import { usePageVisible } from '../utils/visibility';
import styles from './ExperimentsPage.module.css';

/**
 * Read-only monitor of the always-on upload statistics experiment.
 *
 * The experiment parameters live in the configuration file and are edited on
 * the configuration page; this page only shows the settings the daemon is
 * running with and the live real-versus-reported counters.
 */
export function ExperimentsPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const query = useExperiment(auth.api, auth.isReady && visible);

  const view = query.data;
  const settings = view?.settings;
  const totals = view?.totals;
  const modeDescription = experimentModeOption(settings?.mode)?.description;

  return (
    <div className={styles.page}>
      <section className={styles.heading} aria-labelledby="experiments-title">
        <div>
          <p className="eyebrow">反作弊测试</p>
          <h1 className={styles.title} id="experiments-title">
            <IconFlask size={24} aria-hidden="true" /> 上传实验监控
          </h1>
          <p className={styles.subtitle}>
            上传实验常驻运行，默认处于基线模式。这里展示向 tracker 上报的上传量与本机真实传输量，
            用于自建 tracker 的反作弊测试。实验参数在
            <Link to="/config#experiments">配置页面的实验分类</Link>
            （<code className={styles.code}>experiments.upload</code> 节）中维护。
          </p>
        </div>
        <Group gap="xs" align="center">
          {settings !== undefined && (
            <Badge size="lg" variant="light" color="torrent">{experimentModeLabel(settings.mode)}</Badge>
          )}
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
      </section>

      {query.isPending && (
        <div className={`panel panel--padding ${styles.loading}`} role="status" aria-busy="true" aria-label="正在读取上传实验状态">
          <Loader size="sm" />
          <span>正在读取上传实验状态…</span>
        </div>
      )}

      {query.isError && view === undefined && !query.isPending && (
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} title="无法读取上传实验状态" className={styles.alert}>
          <Group justify="space-between" gap="sm">
            <span>后台服务暂时没有返回实验状态，请稍后重试。</span>
            <Button variant="light" color="torrent" size="compact-sm" onClick={() => void query.refetch()}>重新读取</Button>
          </Group>
        </Alert>
      )}

      {query.isError && view !== undefined && (
        <div className="refresh-warning" role="alert">
          <span aria-hidden="true">!</span>
          <span>最新实验状态刷新失败，当前显示上一次有效快照。</span>
        </div>
      )}

      {view !== undefined && settings !== undefined && (
        <div className={styles.layout}>
          <section className={`panel panel--padding ${styles.meta}`} aria-label="当前实验参数">
            <div className="section-heading">
              <div>
                <p className="eyebrow">实验配置</p>
                <h2>当前实验</h2>
              </div>
            </div>
            <dl className={styles.metaList}>
              <div><dt>实验模式</dt><dd>{experimentModeLabel(settings.mode)}</dd></div>
              {settings.mode === 'inject' && (
                <div><dt>虚报速率</dt><dd className="text-mono">{formatUploadRate(settings.rate_bytes_per_second)}</dd></div>
              )}
              {settings.mode === 'amplify' && (
                <div><dt>放大倍率</dt><dd className="text-mono">×{settings.multiplier}</dd></div>
              )}
              <div><dt>抖动百分比</dt><dd>{settings.jitter_percent}%</dd></div>
              <div><dt>上报上限</dt><dd>{settings.cap_bytes > 0 ? formatByteQuantity(settings.cap_bytes) : '不封顶'}</dd></div>
              <div><dt>随机种子</dt><dd className="text-mono">{view.seed}</dd></div>
              <div><dt>Announce 日志</dt><dd className="text-mono">{view.announce_log || '未开启'}</dd></div>
              <div><dt>Peer 采样间隔</dt><dd>{formatPeerSampleInterval(view.peer_sample_interval_seconds)}</dd></div>
            </dl>
            {modeDescription !== undefined && <p className={styles.modeHint}>{modeDescription}</p>}
          </section>

          <section className={`panel panel--padding ${styles.meta}`} aria-label="实验实时数据">
            <div className="section-heading">
              <div>
                <p className="eyebrow">实时数据</p>
                <h2>真实与上报对照</h2>
              </div>
            </div>
            <dl className={styles.metaList}>
              <div><dt>实时真实上传</dt><dd className="text-mono">{formatBytes(totals?.real_uploaded_bytes ?? 0)}</dd></div>
              <div><dt>上次上报时真实</dt><dd className="text-mono">{formatBytes(totals?.real_uploaded_at_last_announce_bytes ?? 0)}</dd></div>
              <div><dt>Tracker 上报</dt><dd className="text-mono">{formatBytes(totals?.reported_uploaded_bytes ?? 0)}</dd></div>
              <div><dt>虚报量</dt><dd className="text-mono">{formatSignedBytes(totals?.overreported_bytes ?? 0)}</dd></div>
              <div><dt>待上报真实增量</dt><dd className="text-mono">{formatBytes(totals?.pending_real_upload_bytes ?? 0)}</dd></div>
            </dl>
            <p className={styles.modeHint}>
              上报值只在 tracker announce 时更新；虚报量按上次上报时点对比，实时真实量在两次 announce 之间会领先于上报值。
            </p>
            <Group gap="xs" className={styles.refreshRow}>
              <Button variant="subtle" size="compact-sm" leftSection={<IconRefresh size={15} />} onClick={() => void query.refetch()} loading={query.isFetching && !query.isPending}>
                刷新状态
              </Button>
            </Group>
          </section>
        </div>
      )}
    </div>
  );
}
