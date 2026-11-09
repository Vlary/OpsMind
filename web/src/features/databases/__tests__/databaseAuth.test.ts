// 评审 verified：编辑无认证（has_password=false）的 MongoDB 时，username
// 校验器恒抛错且「清除认证」复选框不渲染，任何修改都无法保存——死锁。
import { describe, expect, it } from 'vitest';

import { mongoAuthUsernameError } from '../DatabaseFormDrawer';

describe('mongoAuthUsernameError（MongoDB username 校验核心）', () => {
  it('编辑无认证库（has_password=false）：username 留空不拦截（死锁回归）', () => {
    // 改名/换端口/SSL 等任何修改都走得到该分支，此前恒抛错。
    expect(mongoAuthUsernameError(true, false, '', '', false)).toBeNull();
  });

  it('编辑已保存认证的库：清空 username 须显式勾选清除（契约 2.6 保持）', () => {
    expect(mongoAuthUsernameError(true, true, '', '', false)).toBe(
      '清空用户名即清除认证，请勾选「清除已保存的认证」',
    );
    expect(mongoAuthUsernameError(true, true, '', '', true)).toBeNull();
  });

  it('创建路径：成对校验（有用户名无密码拦截；同空放行无认证部署）', () => {
    expect(mongoAuthUsernameError(false, false, 'admin', '', false)).toBe(
      '填写用户名时需同时填写密码；无认证部署则两者都留空',
    );
    expect(mongoAuthUsernameError(false, false, '', '', false)).toBeNull();
  });

  // p3 sprint 3：编辑无认证库只填用户名保存曾落进后端 400（英文 message
  // "mongodb username and password must both be set or both empty" 经顶部
  // 横幅直达用户，username 字段无行内错误，实测走查 sprint 3）——补
  // editing 分支与创建态同文案本地拦截。
  it('编辑无认证库（has_password=false）：只填用户名留空密码本地拦截', () => {
    expect(mongoAuthUsernameError(true, false, 'admin', '', false)).toBe(
      '填写用户名时需同时填写密码；无认证部署则两者都留空',
    );
  });

  it('编辑已保存认证的库：留空密码 = 保留已存密码，只填用户名不拦截', () => {
    expect(mongoAuthUsernameError(true, true, 'admin', '', false)).toBeNull();
    // 成对填入新认证自然放行。
    expect(mongoAuthUsernameError(true, true, 'admin', 'secret', false)).toBeNull();
  });

  it('填密码必须同时填用户名（双向成对）', () => {
    expect(mongoAuthUsernameError(true, true, '', 'secret', false)).toBe(
      '填写密码时需同时填写用户名（认证成对）',
    );
    expect(mongoAuthUsernameError(false, false, '', 'secret', false)).toBe(
      '填写密码时需同时填写用户名（认证成对）',
    );
  });
});
