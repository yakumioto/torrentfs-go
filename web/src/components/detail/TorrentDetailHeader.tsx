import { Button } from '@mantine/core';
import { IconArrowLeft, IconTrash } from '@tabler/icons-react';
import { memo } from 'react';
import { Link } from 'react-router-dom';
import styles from './TorrentDetailHeader.module.css';

export const TorrentDetailHeader = memo(function TorrentDetailHeader({ name, infoHash, onDelete, deleteDisabled }: {
  name: string;
  infoHash: string;
  onDelete: () => void;
  deleteDisabled: boolean;
}) {
  return (
    <div className={styles.header}>
      <div className={styles.title}>
        <Link to="/" className={styles.backLink}><IconArrowLeft size={15} /> 返回任务列表</Link>
        <p className="eyebrow">任务详情</p>
        <h1>{name || '未命名任务'}</h1>
        <span className={styles.hash}>{infoHash || '信息哈希等待生成'}</span>
      </div>
      <div className={styles.actions}>
        <Button color="danger" variant="light" leftSection={<IconTrash size={16} />} onClick={onDelete} disabled={deleteDisabled}>
          删除任务
        </Button>
      </div>
    </div>
  );
});
