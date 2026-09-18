import { Button, Skeleton } from '@mantine/core';
import { IconInbox, IconRefresh, IconSearch } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import styles from './TorrentList.module.css';
import { TorrentRow } from './TorrentRow';

export function TorrentList({ torrents, loading, error, onRetry, onAdd, totalCount = 0, hasFilters, onClearFilters }: {
  torrents?: Torrent[];
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  onAdd: () => void;
  totalCount?: number;
  hasFilters?: boolean;
  onClearFilters?: () => void;
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
      <div className={styles.header} aria-hidden="true">
        <span>任务</span>
        <span>大小</span>
        <span>缓存占用</span>
        <span>状态</span>
        <span>添加时间</span>
        <span>操作</span>
      </div>
      {torrents?.map((torrent) => <TorrentRow key={torrent.id} torrent={torrent} />)}
    </div>
  );
}
