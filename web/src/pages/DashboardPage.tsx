import { Button, TextInput } from '@mantine/core';
import { IconPlus, IconSearch, IconX } from '@tabler/icons-react';
import { useMemo, useState } from 'react';
import { useOutletContext } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import { userFacingError } from '../utils/user-facing-error';
import type { AppOutletContext } from '../components/layout/AppLayout';
import { TorrentList } from '../components/torrents/TorrentList';
import { useTorrentList } from '../queries/hooks';
import { filterTorrents, summarizeTorrents, type TorrentFilter } from '../queries/filter';
import styles from './DashboardPage.module.css';
import { usePageVisible } from '../utils/visibility';

const FILTERS: Array<{ value: TorrentFilter; label: string }> = [
  { value: 'all', label: '全部' },
  { value: 'ready', label: '就绪' },
  { value: 'error', label: '错误' },
];

export function DashboardPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const { openAddTorrent } = useOutletContext<AppOutletContext>();
  const [search, setSearch] = useState('');
  const [filter, setFilter] = useState<TorrentFilter>('all');
  const query = useTorrentList(auth.api, auth.isReady && visible);
  const sourceTorrents = query.data;
  const torrents = useMemo(() => filterTorrents(sourceTorrents ?? [], search, filter), [filter, search, sourceTorrents]);
  const summary = useMemo(() => summarizeTorrents(sourceTorrents ?? []), [sourceTorrents]);
  const hasFilters = search.trim() !== '' || filter !== 'all';

  const clearFilters = () => {
    setSearch('');
    setFilter('all');
  };

  return (
    <div className={styles.page}>
      <section className={styles.heading} aria-labelledby="dashboard-title">
        <div>
          <p className="eyebrow">任务库</p>
          <h1 className={styles.title} id="dashboard-title">任务列表</h1>
          <p className={styles.subtitle}>集中管理任务，并查看真实的缓存占用状态。</p>
        </div>
        <Button color="torrent" leftSection={<IconPlus size={17} />} onClick={openAddTorrent}>
          添加任务
        </Button>
      </section>

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
      />
    </div>
  );
}
