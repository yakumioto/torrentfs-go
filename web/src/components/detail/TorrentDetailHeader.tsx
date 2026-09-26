import { Button, Tooltip } from '@mantine/core';
import { IconArrowLeft, IconTrash, IconUpload } from '@tabler/icons-react';
import { memo } from 'react';
import { Link } from 'react-router-dom';
import styles from './TorrentDetailHeader.module.css';

export const TorrentDetailHeader = memo(function TorrentDetailHeader({ name, infoHash, onDelete, deleteDisabled, onUploadSubtitle, uploadDisabled, uploadDisabledReason }: {
  name: string;
  infoHash: string;
  onDelete: () => void;
  deleteDisabled: boolean;
  onUploadSubtitle: () => void;
  uploadDisabled: boolean;
  uploadDisabledReason: string;
}) {
  const uploadButton = (
    <Button
      color="torrent"
      variant="light"
      leftSection={<IconUpload size={16} />}
      onClick={onUploadSubtitle}
      disabled={uploadDisabled}
      aria-label="上传字幕"
    >
      上传字幕
    </Button>
  );
  return (
    <div className={styles.header}>
      <div className={styles.title}>
        <Link to="/" className={styles.backLink}><IconArrowLeft size={15} /> 返回任务列表</Link>
        <p className="eyebrow">任务详情</p>
        <h1>{name || '未命名任务'}</h1>
        <span className={styles.hash}>{infoHash || '信息哈希等待生成'}</span>
      </div>
      <div className={styles.actions}>
        <div className={styles.actionGroup}>
          {uploadDisabled && uploadDisabledReason !== ''
            ? <Tooltip label={uploadDisabledReason} withArrow><span className={styles.tooltipAnchor}>{uploadButton}</span></Tooltip>
            : uploadButton}
          {uploadDisabled && uploadDisabledReason !== '' && <span className={styles.actionHint}>{uploadDisabledReason}</span>}
        </div>
        <Button color="danger" variant="light" leftSection={<IconTrash size={16} />} onClick={onDelete} disabled={deleteDisabled}>
          删除任务
        </Button>
      </div>
    </div>
  );
});
