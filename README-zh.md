<div align="center">
  <img height="150" src="logo.png"  />
</div>

###

<h1 align="center">pipiGo</h1>

###

极简接口请求定时任务管理器

源自我的一个需求，能够每3h请求一下dify工作流的api接口，执行一些对应的任务，需要有可视化的webui。

找了很多开源框架，功能够很全也很臃肿，我只想要这个功能，如果能够增删改查任务就能更好了，然后找到了大模型，他拍了一下我的头，说：开悟了。于是就有了这个项目。

### 部署

```bash
docker compose build

docker compose up -d
```

任务数据自动保存到sqlite文件中。

### ui

![创建任务](./screenshot/ui-1.png)

![运行任务](./screenshot/ui-2.png)
