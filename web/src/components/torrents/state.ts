import { IconAlertTriangle, IconCircleCheck, IconLoader, IconQuestionMark } from '@tabler/icons-react';

interface StateMeta {
  label: string;
  icon: typeof IconLoader;
}

export const stateMeta: Record<string, StateMeta> = {
  adding: { label: '添加中', icon: IconLoader },
  ready: { label: '就绪', icon: IconCircleCheck },
  error: { label: '错误', icon: IconAlertTriangle },
  deleting: { label: '删除中', icon: IconLoader },
  delete_failed: { label: '删除失败', icon: IconAlertTriangle },
  unknown: { label: '未知状态', icon: IconQuestionMark },
};
