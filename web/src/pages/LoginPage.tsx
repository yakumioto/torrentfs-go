import { Button, PasswordInput, TextInput } from '@mantine/core';
import { IconKey, IconLock, IconRefresh } from '@tabler/icons-react';
import { type FormEvent, useState } from 'react';
import { useAuth } from '../app/auth-context';

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
    <main className="auth-page">
      <section className="auth-card" aria-labelledby="login-title">
        <div className="auth-card__mark" aria-hidden="true"><IconLock size={22} /></div>
        <p className="eyebrow">TorrentFS</p>
        <h1 id="login-title">Sign in to TorrentFS</h1>
        <p className="auth-card__copy">Connect to your local torrent daemon to manage downloads and inspect their live state.</p>
        <form onSubmit={submit}>
          <TextInput
            label="Username"
            value={username}
            onChange={(event) => setUsername(event.currentTarget.value)}
            autoComplete="username"
            required
            mb="md"
          />
          <PasswordInput
            label="Password"
            value={password}
            onChange={(event) => setPassword(event.currentTarget.value)}
            autoComplete="current-password"
            required
            mb="lg"
          />
          {auth.sessionNotice !== '' && <div className="session-callout" role="status" aria-live="polite"><IconRefresh size={15} aria-hidden="true" /> {auth.sessionNotice}</div>}
          {auth.loginError !== '' && <div className="error-callout" role="alert" aria-live="polite"><IconKey size={15} aria-hidden="true" /> {auth.loginError}</div>}
          <Button type="submit" fullWidth color="mint" loading={submitting} mt="lg">Sign in</Button>
        </form>
        <p className="auth-card__footer">Your session is kept until this browser tab is closed.</p>
      </section>
    </main>
  );
}
