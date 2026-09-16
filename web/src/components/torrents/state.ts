import { IconAlertTriangle, IconCircleCheck, IconDownload, IconLoader, IconQuestionMark } from '@tabler/icons-react';

interface StateMeta {
  label: string;
  className: string;
  icon: typeof IconLoader;
}

export const stateMeta: Record<string, StateMeta> = {
  adding: { label: 'Adding', className: 'state-badge--adding', icon: IconLoader },
  downloading: { label: 'Downloading', className: 'state-badge--downloading', icon: IconDownload },
  seeding: { label: 'Seeding', className: 'state-badge--seeding', icon: IconCircleCheck },
  error: { label: 'Error', className: 'state-badge--error', icon: IconAlertTriangle },
  deleting: { label: 'Deleting', className: 'state-badge--deleting', icon: IconLoader },
  delete_failed: { label: 'Delete failed', className: 'state-badge--delete-failed', icon: IconAlertTriangle },
  unknown: { label: 'Unknown', className: '', icon: IconQuestionMark },
};
