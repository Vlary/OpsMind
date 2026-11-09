import { ApiOutlined } from '@ant-design/icons';
import { Button } from 'antd';

interface ServerTestButtonProps {
  loading: boolean;
  onClick: () => void;
}

// 服务器连接测试按钮：SSH 通道只做握手；WinRM 通道执行只读探针（契约 2.7）。
export default function ServerTestButton({ loading, onClick }: ServerTestButtonProps) {
  return (
    <Button size="small" type="link" icon={<ApiOutlined />} loading={loading} onClick={onClick}>
      测试
    </Button>
  );
}
