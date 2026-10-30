# The build context is sample/services/dotnet, so build from there:
#   docker build -f Worker/Dockerfile .
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /src
# Restore first, from the project files alone, so code changes reuse the cached packages.
COPY Directory.Build.props Directory.Packages.props ./
COPY Shared/Shared.csproj Shared/
COPY Worker/Worker.csproj Worker/
RUN dotnet restore Worker/Worker.csproj
COPY Shared/ Shared/
COPY Worker/ Worker/
RUN dotnet publish Worker/Worker.csproj --configuration Release --no-restore --output /app

FROM mcr.microsoft.com/dotnet/runtime:10.0
WORKDIR /app
COPY --from=build /app .
USER $APP_UID
ENTRYPOINT ["dotnet", "Worker.dll"]
