import { IconQuestionMark } from '@tabler/icons-react';
import { stateMeta } from './state';
import styles from './StateBadge.module.css';

const VARIANT_CLASS: Record<string, string> = {
  adding: styles.adding,
  ready: styles.ready,
  error: styles.error,
  deleting: styles.deleting,
  delete_failed: styles.deleteFailed,
};

export function StateBadge({ state, className }: { state: string; className?: string }) {
  const meta = stateMeta[state] ?? { label: '未知状态', icon: IconQuestionMark };
  const Icon = meta.icon;
  return (
    <span className={[styles.badge, VARIANT_CLASS[state], className].filter(Boolean).join(' ')}>
      <Icon size={13} aria-hidden="true" />
      <span>{meta.label}</span>
      <span className={styles.dot} aria-hidden="true" />
    </span>
  );
}
