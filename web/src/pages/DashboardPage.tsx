import { Button, TextInput } from '@mantine/core';
import { IconPlus, IconSearch, IconX } from '@tabler/icons-react';
import { useMemo, useState } from 'react';
import { useOutletContext } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import type { AppOutletContext } from '../components/layout/AppLayout';
import { TorrentList } from '../components/torrents/TorrentList';
import { useTorrentList } from '../queries/hooks';
import { filterTorrents, summarizeTorrents, type TorrentFilter } from '../queries/filter';
import { errorMessage } from '../api/errors';
import styles from './DashboardPage.module.css';
import { usePageVisible } from '../utils/visibility';

const FILTERS: Array<{ value: TorrentFilter; label: string }> = [
  { value: 'all', label: 'All' },
  { value: 'downloading', label: 'Downloading' },
  { value: 'seeding', label: 'Seeding' },
  { value: 'completed', label: 'Completed' },
  { value: 'error', label: 'Error' },
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
          <p className="eyebrow">Library</p>
          <h1 className={styles.title} id="dashboard-title">Torrents</h1>
          <p className={styles.subtitle}>Manage downloads and inspect live progress from one focused queue.</p>
        </div>
        <Button color="mint" leftSection={<IconPlus size={17} />} onClick={openAddTorrent}>
          Add torrent
        </Button>
      </section>

      <section aria-label="Torrent summary" className={styles.summary}>
        <span><strong>{summary.total}</strong> total</span>
        <span><strong>{summary.downloading}</strong> downloading</span>
        <span><strong>{summary.seeding}</strong> seeding</span>
        <span><strong>{summary.completed}</strong> completed</span>
        <span><strong>{summary.error}</strong> error</span>
      </section>

      <section className={styles.controls} aria-label="Torrent list controls">
        <TextInput
          className={styles.search}
          aria-label="Search torrents"
          placeholder="Search name or info hash"
          leftSection={<IconSearch size={16} />}
          rightSection={search !== '' ? <Button variant="subtle" size="compact-xs" aria-label="Clear search" onClick={() => setSearch('')}><IconX size={14} /></Button> : null}
          value={search}
          onChange={(event) => setSearch(event.currentTarget.value)}
        />
        <div className={styles.filters} role="group" aria-label="Filter torrents by status">
          {FILTERS.map((item) => (
            <Button
              key={item.value}
              size="compact-sm"
              variant={filter === item.value ? 'filled' : 'subtle'}
              color="mint"
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
          <span>Could not refresh the latest snapshot. Showing the last valid list. {errorMessage(query.error)}</span>
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
