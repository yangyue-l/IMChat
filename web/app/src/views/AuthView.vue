<script setup>
import { reactive, ref } from 'vue';
import { state, submitAuth, toggleTheme } from '../store';
import Icon from '../components/Icon.vue';

const mode = ref('login');
const busy = ref(false);
const error = ref('');
const form = reactive({ username: '', password: '', nickname: '' });

function switchMode() {
  mode.value = mode.value === 'login' ? 'register' : 'login';
  error.value = '';
}

async function submit() {
  if (busy.value) return;
  if (!form.username.trim() || !form.password) {
    error.value = '请填写用户名和密码';
    return;
  }
  if (mode.value === 'register' && form.password.length < 6) {
    error.value = '密码至少 6 位';
    return;
  }

  busy.value = true;
  error.value = '';
  try {
    const username = form.username.trim();
    const payload = mode.value === 'register'
      ? {
        username,
        password: form.password,
        ...(form.nickname.trim() ? { nickname: form.nickname.trim() } : {}),
      }
      : { username, password: form.password, device: 'web' };
    await submitAuth(mode.value, payload);
  } catch (err) {
    error.value = err.message;
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="auth">
    <aside class="auth-visual">
      <div class="auth-brand">
        <div class="mark">IM</div>
        <b>IMChat</b>
      </div>

      <div class="auth-copy">
        <h1>把对话，留在该在的地方。</h1>
        <p>私聊、群聊、朋友圈。一套轻量但完整的即时通讯体验，消息实时抵达。</p>
      </div>

      <div class="auth-preview" aria-hidden="true">
        <div class="auth-bubble"><span class="who">林小满</span>晚上那家店还去吗？</div>
        <div class="auth-bubble me">去，七点见。</div>
        <div class="auth-bubble"><span class="who">林小满</span>好，我先订位。</div>
      </div>

      <div class="auth-stack">
        <span>Go</span><span>Gin</span><span>MySQL</span><span>Redis</span>
        <span>RabbitMQ</span><span>WebSocket</span>
      </div>
    </aside>

    <div class="auth-panel">
      <button class="icon-btn auth-theme" title="切换主题" aria-label="切换主题" @click="toggleTheme">
        <Icon :name="state.theme === 'dark' ? 'sun' : 'moon'" />
      </button>

      <form class="auth-form" novalidate @submit.prevent="submit">
        <h2>{{ mode === 'login' ? '欢迎回来' : '创建账号' }}</h2>
        <p class="sub">{{ mode === 'login' ? '登录以继续你的对话' : '几秒钟就能开始聊天' }}</p>

        <label class="field">
          <span>用户名</span>
          <input
            v-model="form.username"
            autocomplete="username"
            placeholder="3-20 位字母、数字或下划线"
          />
        </label>

        <label class="field">
          <span>密码</span>
          <input
            v-model="form.password"
            type="password"
            :autocomplete="mode === 'login' ? 'current-password' : 'new-password'"
            placeholder="至少 6 位"
          />
        </label>

        <label v-if="mode === 'register'" class="field">
          <span>昵称（可选）</span>
          <input v-model="form.nickname" autocomplete="nickname" placeholder="不填则使用用户名" />
        </label>

        <button class="btn primary block" type="submit" :disabled="busy">
          {{ busy ? (mode === 'login' ? '登录中…' : '注册中…') : (mode === 'login' ? '登录' : '注册并登录') }}
        </button>

        <div class="auth-error" role="alert" aria-live="polite">{{ error }}</div>

        <div class="auth-switch">
          <span>{{ mode === 'login' ? '还没有账号？' : '已经有账号了？' }}</span>
          <button type="button" @click="switchMode">{{ mode === 'login' ? '立即注册' : '去登录' }}</button>
        </div>

        <div class="auth-foot">IMChat · 单文件构建，零运行时依赖</div>
      </form>
    </div>
  </div>
</template>
