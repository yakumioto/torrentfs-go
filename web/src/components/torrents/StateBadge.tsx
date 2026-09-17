import { IconQuestionMark } from '@tabler/icons-react';
import { stateMeta } from './state';

export function StateBadge({ state }: { state: string }) {
  const meta = stateMeta[state] ?? { label: 'Unknown', className: '', icon: IconQuestionMark };
  const Icon = meta.icon;
  return (
    <span className={`state-badge ${meta.className}`}>
      <Icon size={13} aria-hidden="true" />
      <span>{meta.label}</span>
      <span className="state-badge__dot" aria-hidden="true" />
    </span>
  );
}
