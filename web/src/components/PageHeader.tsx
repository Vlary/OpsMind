import { Flex, Typography } from 'antd';
import type { ReactNode } from 'react';

interface PageHeaderProps {
  title: ReactNode;
  description?: ReactNode;
  extra?: ReactNode;
}

export default function PageHeader({ title, description, extra }: PageHeaderProps) {
  return (
    <Flex align="flex-start" justify="space-between" gap={16} style={{ marginBottom: 20 }}>
      <div>
        <Typography.Title level={3} style={{ margin: 0 }}>
          {title}
        </Typography.Title>
        {description ? <Typography.Text type="secondary">{description}</Typography.Text> : null}
      </div>
      {extra ? <div>{extra}</div> : null}
    </Flex>
  );
}
