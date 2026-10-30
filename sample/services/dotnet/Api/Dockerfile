# The build context is sample/services/dotnet, so build from there:
#   docker build -f Api/Dockerfile .
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /src
# Restore first, from the project files alone, so code changes reuse the cached packages.
COPY Directory.Build.props Directory.Packages.props ./
COPY Shared/Shared.csproj Shared/
COPY Api/Api.csproj Api/
RUN dotnet restore Api/Api.csproj
COPY Shared/ Shared/
COPY Api/ Api/
RUN dotnet publish Api/Api.csproj --configuration Release --no-restore --output /app

FROM mcr.microsoft.com/dotnet/aspnet:10.0
WORKDIR /app
COPY --from=build /app .
USER $APP_UID
# Port 8080 serves gRPC and HTTP over cleartext HTTP/2, and port 9090 serves /metrics
# over HTTP/1.1.
EXPOSE 8080 9090
ENTRYPOINT ["dotnet", "Api.dll"]
