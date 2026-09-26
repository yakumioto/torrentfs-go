import { Button, Group, Skeleton, TextInput } from '@mantine/core';
import { IconPlus, IconSearch, IconTrashX, IconX } from '@tabler/icons-react';
import { useMemo, useState } from 'react';
import { useOutletContext } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import { userFacingError } from '../utils/user-facing-error';
import type { AppOutletContext } from '../components/layout/AppLayout';
import { PruneTorrentsDialog } from '../components/dialogs/PruneTorrentsDialog';
import { TorrentList } from '../components/torrents/TorrentList';
import { useRuntimeStats, useTorrentList } from '../queries/hooks';
import { filterTorrents, summarizeTorrents, type TorrentFilter } from '../queries/filter';
import {
  DEFAULT_TORRENT_SORT,
  defaultSortDirection,
  sortTorrents,
  type TorrentSort,
  type TorrentSortKey,
} from '../queries/sort';
import { formatBytes, formatDate, percent } from '../utils/format';
import styles from './DashboardPage.module.css';
import { usePageVisible } from '../utils/visibility';

const FILTERS: Array<{ value: TorrentFilter; label: string }> = [
  { value: 'all', label: '全部' },
  { value: 'ready', label: '就绪' },
  { value: 'error', label: '错误' },
  { value: 'favorite', label: '收藏' },
];

export function DashboardPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const { openAddTorrent } = useOutletContext<AppOutletContext>();
  const [search, setSearch] = useState('');
  const [filter, setFilter] = useState<TorrentFilter>('all');
  const [sort, setSort] = useState<TorrentSort>(DEFAULT_TORRENT_SORT);
  const [pruneOpen, setPruneOpen] = useState(false);
  const query = useTorrentList(auth.api, auth.isReady && visible);
  const statsQuery = useRuntimeStats(auth.api, auth.isReady && visible);
  const sourceTorrents = query.data;
  const filteredTorrents = useMemo(() => filterTorrents(sourceTorrents ?? [], search, filter), [filter, search, sourceTorrents]);
  const torrents = useMemo(() => sortTorrents(filteredTorrents, sort), [filteredTorrents, sort]);
  const summary = useMemo(() => summarizeTorrents(sourceTorrents ?? []), [sourceTorrents]);
  const hasFilters = search.trim() !== '' || filter !== 'all';

  const clearFilters = () => {
    setSearch('');
    setFilter('all');
  };

  const changeSortKey = (key: TorrentSortKey) => {
    setSort((current) => current.key === key
      ? { ...current, direction: current.direction === 'asc' ? 'desc' : 'asc' }
      : { key, direction: defaultSortDirection(key) });
  };

  const toggleSortDirection = () => {
    setSort((current) => ({ ...current, direction: current.direction === 'asc' ? 'desc' : 'asc' }));
  };

  return (
    <div className={styles.page}>
      <section className={styles.heading} aria-labelledby="dashboard-title">
        <div>
          <p className="eyebrow">任务库</p>
          <h1 className={styles.title} id="dashboard-title">任务列表</h1>
          <p className={styles.subtitle}>集中管理任务，查看全局缓存与本次后端启动以来的传输统计。</p>
        </div>
        <Group gap="xs" className={styles.headingActions}>
          <Button variant="light" color="torrent" leftSection={<IconTrashX size={17} />} onClick={() => setPruneOpen(true)}>
            批量清理
          </Button>
          <Button color="torrent" leftSection={<IconPlus size={17} />} onClick={openAddTorrent}>
            添加任务
          </Button>
        </Group>
      </section>

      <section aria-label="运行时统计" className={styles.runtimeStats}>
        {statsQuery.data === undefined && statsQuery.isPending && (
          <div className={styles.runtimeLoading} role="status" aria-busy="true" aria-label="正在加载运行时统计">
            {[0, 1, 2].map((item) => <Skeleton key={item} height={94} radius="md" />)}
          </div>
        )}
        {statsQuery.data !== undefined && (
          <>
            <RuntimeCard
              label="全局缓存"
              value={`${formatBytes(statsQuery.data.cache.used_bytes)} / ${formatBytes(statsQuery.data.cache.capacity_bytes)}`}
              detail={`占用 ${cacheUsagePercent(statsQuery.data.cache.used_bytes, statsQuery.data.cache.capacity_bytes).toFixed(1)}% · 自 ${formatDate(statsQuery.data.started_at)}`}
            />
            <RuntimeCard
              label="本次启动下载"
              value={formatBytes(statsQuery.data.transfer.downloaded_bytes)}
              detail={`有效下载 payload · 自 ${formatDate(statsQuery.data.started_at)}`}
            />
            <RuntimeCard
              label="本次启动上传"
              value={formatBytes(statsQuery.data.transfer.uploaded_bytes)}
              detail={`上传 payload · 自 ${formatDate(statsQuery.data.started_at)}`}
            />
          </>
        )}
        {statsQuery.data === undefined && !statsQuery.isPending && (
          <div className={styles.runtimeUnavailable} role="status">
            <strong>运行时统计暂不可用</strong>
            <span>{userFacingError(statsQuery.error, '统计接口不可用，任务列表仍可正常使用。')}</span>
          </div>
        )}
      </section>

      {statsQuery.error !== null && statsQuery.error !== undefined && statsQuery.data !== undefined && (
        <div className="refresh-warning" role="alert">
          <span aria-hidden="true">!</span>
          <span>最新运行时统计刷新失败，当前显示上一次有效快照。{userFacingError(statsQuery.error, '请稍后重试。')}</span>
        </div>
      )}

      <section aria-label="任务摘要" className={styles.summary}>
        <div className={styles.summaryCard}>
          <span>全部任务</span>
          <strong>{summary.total}</strong>
        </div>
        <div className={`${styles.summaryCard} ${styles.summaryReady}`}>
          <span>已就绪</span>
          <strong>{summary.ready}</strong>
        </div>
        <div className={`${styles.summaryCard} ${styles.summaryError}`}>
          <span>需要关注</span>
          <strong>{summary.error}</strong>
        </div>
      </section>

      <section className={styles.controls} aria-label="任务列表筛选">
        <TextInput
          className={styles.search}
          aria-label="搜索任务"
          placeholder="搜索任务名称或信息哈希"
          leftSection={<IconSearch size={16} />}
          rightSection={search !== '' ? <Button variant="subtle" size="compact-xs" aria-label="清除搜索" onClick={() => setSearch('')}><IconX size={14} /></Button> : null}
          value={search}
          onChange={(event) => setSearch(event.currentTarget.value)}
        />
        <div className={styles.filters} role="group" aria-label="按状态筛选任务">
          {FILTERS.map((item) => (
            <Button
              key={item.value}
              size="compact-sm"
              variant={filter === item.value ? 'filled' : 'subtle'}
              color="torrent"
              aria-pressed={filter === item.value}
              onClick={() => setFilter(item.value)}
            >
              {item.label}
            </Button>
          ))}
        </div>
      </section>

      {query.error !== null && query.error !== undefined && query.data !== undefined && (
        <div className="refresh-warning" role="alert">
          <span aria-hidden="true">!</span>
          <span>最新任务列表刷新失败，当前显示上一次有效快照。{userFacingError(query.error, '请稍后重试。')}</span>
        </div>
      )}

      <TorrentList
        torrents={query.data === undefined ? undefined : torrents}
        totalCount={summary.total}
        loading={query.isPending}
        error={query.error}
        onRetry={() => void query.refetch()}
        onAdd={openAddTorrent}
        hasFilters={hasFilters}
        onClearFilters={clearFilters}
        sort={sort}
        onSortKeyChange={changeSortKey}
        onSortDirectionToggle={toggleSortDirection}
      />
      <PruneTorrentsDialog opened={pruneOpen} onClose={() => setPruneOpen(false)} />
    </div>
  );
}

function RuntimeCard({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <div className={styles.runtimeCard}>
      <span>{label}</span>
      <strong>{value}</strong>
      <small>{detail}</small>
    </div>
  );
}

function cacheUsagePercent(used: number, capacity: number): number {
  if (!Number.isFinite(used) || !Number.isFinite(capacity) || capacity <= 0) {
    return 0;
  }
  return percent(used / capacity);
}
