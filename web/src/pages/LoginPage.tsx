import { Button, PasswordInput, TextInput } from '@mantine/core';
import { IconBox, IconKey, IconLock, IconRefresh } from '@tabler/icons-react';
import { type FormEvent, useState } from 'react';
import { useAuth } from '../app/auth-context';
import shell from '../styles/auth-shell.module.css';
import styles from './LoginPage.module.css';

export function LoginPage() {
  const auth = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSubmitting(true);
    const success = await auth.login(username, password);
    setSubmitting(false);
    if (success) {
      setPassword('');
    }
  };

  return (
    <main className={`${shell.page} ${shell.authPage}`}>
      <section className={shell.brandPanel} aria-label="TorrentFS 简介">
        <span className={shell.brandMark} aria-hidden="true"><IconBox size={24} /></span>
        <p className="eyebrow">TorrentFS</p>
        <h1>清晰管理每一个任务。</h1>
        <p>在本地服务中整理磁力链接、种子文件和缓存状态，随时查看真实的数据块信息。</p>
      </section>
      <section className={`${shell.card} ${styles.card}`} aria-labelledby="login-title">
        <div className={styles.mark} aria-hidden="true"><IconLock size={22} /></div>
        <p className="eyebrow">安全连接</p>
        <h2 id="login-title">登录 TorrentFS</h2>
        <p className={styles.copy}>连接到本地 TorrentFS 服务后即可查看和管理任务。</p>
        <form onSubmit={submit}>
          <TextInput
            label="用户名"
            value={username}
            onChange={(event) => setUsername(event.currentTarget.value)}
            autoComplete="username"
            required
            mb="md"
          />
          <PasswordInput
            label="密码"
            value={password}
            onChange={(event) => setPassword(event.currentTarget.value)}
            autoComplete="current-password"
            visibilityToggleButtonProps={{ 'aria-label': '显示或隐藏密码' }}
            required
            mb="lg"
          />
          {auth.sessionNotice !== '' && <div className={styles.sessionCallout} role="status" aria-live="polite"><IconRefresh size={15} aria-hidden="true" /> {auth.sessionNotice}</div>}
          {auth.loginError !== '' && <div className="error-callout" role="alert" aria-live="polite"><IconKey size={15} aria-hidden="true" /> {auth.loginError}</div>}
          <Button type="submit" fullWidth color="torrent" loading={submitting} mt="lg">登录</Button>
        </form>
        <p className={styles.footer}>会话仅保存在当前浏览器标签页中。</p>
      </section>
    </main>
  );
}
