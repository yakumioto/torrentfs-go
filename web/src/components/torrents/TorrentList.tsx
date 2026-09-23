import { Button, Skeleton } from '@mantine/core';
import { IconChevronDown, IconChevronUp, IconInbox, IconRefresh, IconSearch } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import type { SortDirection, TorrentSort, TorrentSortKey } from '../../queries/sort';
import styles from './TorrentList.module.css';
import { TorrentRow } from './TorrentRow';

const SORT_FIELDS: Array<{ key: TorrentSortKey; label: string }> = [
  { key: 'name', label: '任务' },
  { key: 'total_bytes', label: '大小' },
  { key: 'state', label: '状态' },
  { key: 'created_at', label: '添加时间' },
];

export function TorrentList({
  torrents,
  loading,
  error,
  onRetry,
  onAdd,
  totalCount = 0,
  hasFilters,
  onClearFilters,
  sort,
  onSortKeyChange,
  onSortDirectionToggle,
}: {
  torrents?: Torrent[];
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  onAdd: () => void;
  totalCount?: number;
  hasFilters?: boolean;
  onClearFilters?: () => void;
  sort: TorrentSort;
  onSortKeyChange: (key: TorrentSortKey) => void;
  onSortDirectionToggle: () => void;
}) {
  if (loading && torrents === undefined) {
    return (
      <div className={`panel ${styles.list}`} role="status" aria-busy="true" aria-label="正在加载任务列表">
        {[0, 1, 2].map((item) => <Skeleton key={item} height={88} radius={0} />)}
      </div>
    );
  }

  if (error !== null && error !== undefined && torrents === undefined) {
    return (
      <div className="panel error-state" role="alert">
        <IconRefresh size={30} aria-hidden="true" />
        <h2>任务列表暂不可用</h2>
        <p>后台服务没有返回任务列表，已有任务不会受到影响。</p>
        <Button variant="light" color="torrent" onClick={onRetry}>重新加载</Button>
      </div>
    );
  }

  if (torrents !== undefined && torrents.length === 0) {
    if (hasFilters) {
      return (
        <div className="panel empty-state" role="status">
          <span className="empty-state__mark" aria-hidden="true"><IconSearch size={25} /></span>
          <h2>没有匹配的任务</h2>
          <p>请尝试其他任务名称、信息哈希或状态筛选。</p>
          <Button variant="light" color="torrent" onClick={onClearFilters}>清除搜索和筛选</Button>
        </div>
      );
    }
    return (
      <div className="panel empty-state">
        <span className="empty-state__mark" aria-hidden="true"><IconInbox size={25} /></span>
        <h2>还没有任务</h2>
        <p>添加磁力链接或上传 .torrent 文件，开始管理第一个任务。</p>
        <Button color="torrent" onClick={onAdd}>添加第一个任务</Button>
      </div>
    );
  }

  return (
    <div className={`panel ${styles.list}`} role="list" aria-label={`任务列表${totalCount > 0 ? `，共 ${totalCount} 个任务` : ''}`}>
      <div className={styles.header} role="row">
        {SORT_FIELDS.slice(0, 2).map((field) => (
          <SortHeader
            key={field.key}
            label={field.label}
            sortKey={field.key}
            sort={sort}
            onSortKeyChange={onSortKeyChange}
          />
        ))}
        <div className={styles.headerCell} role="columnheader"><span>下载量</span></div>
        <div className={styles.headerCell} role="columnheader"><span>上传量</span></div>
        {SORT_FIELDS.slice(2).map((field) => (
          <SortHeader
            key={field.key}
            label={field.label}
            sortKey={field.key}
            sort={sort}
            onSortKeyChange={onSortKeyChange}
          />
        ))}
        <div className={styles.headerCell} role="columnheader"><span>操作</span></div>
      </div>
      <div className={styles.mobileSort} role="group" aria-label="任务列表排序">
        <label htmlFor="torrent-mobile-sort">排序字段</label>
        <select
          id="torrent-mobile-sort"
          value={sort.key}
          aria-label="移动端排序字段"
          onChange={(event) => onSortKeyChange(event.currentTarget.value as TorrentSortKey)}
        >
          {SORT_FIELDS.map((field) => <option key={field.key} value={field.key}>{field.label}</option>)}
        </select>
        <button
          type="button"
          className={styles.directionButton}
          aria-label={`切换为${sort.direction === 'asc' ? '降序' : '升序'}`}
          onClick={onSortDirectionToggle}
        >
          {sort.direction === 'asc' ? <IconChevronUp size={15} aria-hidden="true" /> : <IconChevronDown size={15} aria-hidden="true" />}
          <span>{sort.direction === 'asc' ? '升序' : '降序'}</span>
        </button>
      </div>
      {torrents?.map((torrent) => <TorrentRow key={torrent.id} torrent={torrent} />)}
    </div>
  );
}

function SortHeader({
  label,
  sortKey,
  sort,
  onSortKeyChange,
}: {
  label: string;
  sortKey: TorrentSortKey;
  sort: TorrentSort;
  onSortKeyChange: (key: TorrentSortKey) => void;
}) {
  const active = sort.key === sortKey;
  const direction: SortDirection = active ? sort.direction : 'asc';
  return (
    <div
      className={styles.headerCell}
      role="columnheader"
      aria-sort={active ? direction === 'asc' ? 'ascending' : 'descending' : 'none'}
    >
      <button
        type="button"
        className={`${styles.sortButton} ${active ? styles.sortButtonActive : ''}`}
        aria-label={`按${label}排序${active ? `，当前${direction === 'asc' ? '升序' : '降序'}` : ''}`}
        onClick={() => onSortKeyChange(sortKey)}
      >
        <span>{label}</span>
        {active && (direction === 'asc' ? <IconChevronUp size={14} aria-hidden="true" /> : <IconChevronDown size={14} aria-hidden="true" />)}
      </button>
    </div>
  );
}
